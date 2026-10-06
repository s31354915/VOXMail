#!/usr/bin/env python3
"""Opt-in local SIP/RTP peer for the packaged VOXMail native smoke test.

This is deliberately a small, deterministic peer rather than a production SIP
proxy.  It implements the subset needed to exercise the pinned Baresip
boundary: REGISTER with RFC 3261 digest authentication, INVITE/ACK/BYE,
PCMU/PCMA plus RFC 4733 telephone-event SDP, and bidirectional RTP.

The test is never run by the normal unit suite.  ``scripts/sip-peer-smoke.sh``
starts it with a real Baresip binary and the real ``voxmail`` module inside an
image.  A failure is an error; a missing Baresip binary is also an error so a
caller cannot mistake a skipped native test for a pass.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import queue
import random
import re
import select
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import quote
from typing import Iterable


TIMEOUT = 12.0
SIP_TIMEOUT = 0.2
RTP_CLOCK = 8000
RTP_PTIME = 0.020
RTP_SAMPLES = int(RTP_CLOCK * RTP_PTIME)
PCMU = 0
PCMA = 8
TELEPHONE_EVENT = 101


class SmokeFailure(RuntimeError):
    pass


def now() -> float:
    return time.monotonic()


def wait_until(predicate, timeout: float, description: str, interval: float = 0.02):
    deadline = now() + timeout
    while now() < deadline:
        if predicate():
            return
        time.sleep(interval)
    raise SmokeFailure(f"timed out waiting for {description}")


def free_udp_port() -> int:
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]
    finally:
        sock.close()


def control_socket_is_dead(path: Path) -> bool:
    if not path.exists():
        return True
    probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    probe.settimeout(0.2)
    try:
        probe.connect(str(path))
    except (ConnectionRefusedError, FileNotFoundError):
        return True
    except OSError:
        return False
    else:
        return False
    finally:
        probe.close()


def md5(value: str) -> str:
    return hashlib.md5(value.encode("utf-8"), usedforsecurity=False).hexdigest()


def parse_header_values(raw: str) -> dict[str, str]:
    result: dict[str, str] = {}
    for part in re.split(r",\s*(?=[A-Za-z][A-Za-z0-9_-]*\s*=|[A-Za-z][A-Za-z0-9_-]*\s*$)", raw):
        if "=" not in part:
            continue
        key, value = part.split("=", 1)
        value = value.strip()
        if value.startswith('"') and value.endswith('"'):
            value = value[1:-1].replace('\\"', '"').replace('\\\\', '\\')
        result[key.strip().lower()] = value
    return result


@dataclass
class SIPMessage:
    start: str
    headers: dict[str, list[str]]
    body: str

    @property
    def method(self) -> str:
        return self.start.split(" ", 1)[0]

    @property
    def status(self) -> int | None:
        if not self.start.startswith("SIP/2.0 "):
            return None
        try:
            return int(self.start.split(" ", 2)[1])
        except (IndexError, ValueError):
            return None

    def header(self, name: str, default: str = "") -> str:
        values = self.headers.get(name.lower(), [])
        return values[0] if values else default


def parse_sip(data: bytes) -> SIPMessage:
    text = data.decode("latin-1")
    head, separator, body = text.partition("\r\n\r\n")
    if not separator:
        head, separator, body = text.partition("\n\n")
    lines = re.split(r"\r?\n", head)
    if not lines or not lines[0]:
        raise SmokeFailure("peer received an empty SIP message")
    headers: dict[str, list[str]] = {}
    for line in lines[1:]:
        if not line or ":" not in line:
            continue
        key, value = line.split(":", 1)
        headers.setdefault(key.strip().lower(), []).append(value.strip())
    length_text = headers.get("content-length", ["0"])[0]
    try:
        length = int(length_text)
    except ValueError as exc:
        raise SmokeFailure(f"invalid SIP Content-Length: {length_text!r}") from exc
    return SIPMessage(lines[0], headers, body[:length])


def header_tag(value: str) -> str:
    match = re.search(r"(?:^|;)tag=([^;\s]+)", value)
    return match.group(1) if match else ""


def add_tag(value: str, tag: str) -> str:
    return value if header_tag(value) else f"{value};tag={tag}"


def uri_user(value: str) -> str:
    match = re.search(r"sip:([^@;>]+)@", value)
    return match.group(1) if match else ""


def parse_rtp_target(body: str) -> tuple[str, int, list[int], int | None]:
    address = "127.0.0.1"
    audio_port = 0
    payloads: list[int] = []
    event_pt: int | None = None
    lines = [line.strip() for line in body.splitlines()]
    for line in lines:
        if line.startswith("c=IN IP4 "):
            address = line.rsplit(" ", 1)[1]
        if line.startswith("m=audio "):
            fields = line.split()
            if len(fields) >= 4:
                audio_port = int(fields[1])
                payloads = [int(value) for value in fields[3:]]
        match = re.match(r"a=rtpmap:(\d+)\s+telephone-event/", line, re.IGNORECASE)
        if match:
            event_pt = int(match.group(1))
    if not audio_port or not payloads:
        raise SmokeFailure(f"SIP SDP has no usable audio target:\n{body}")
    return address, audio_port, payloads, event_pt


def choose_codec(payloads: Iterable[int]) -> tuple[int, str]:
    offered = set(payloads)
    if PCMU in offered:
        return PCMU, "PCMU"
    if PCMA in offered:
        return PCMA, "PCMA"
    raise SmokeFailure(f"peer did not offer PCMU or PCMA: {sorted(offered)}")


def make_sdp(port: int, codec: int, codec_name: str, event_pt: int = TELEPHONE_EVENT) -> str:
    return (
        "v=0\r\n"
        "o=voxmail-test 1 1 IN IP4 127.0.0.1\r\n"
        "s=VOXMail local SIP peer\r\n"
        "c=IN IP4 127.0.0.1\r\n"
        "t=0 0\r\n"
        f"m=audio {port} RTP/AVP {codec} {event_pt}\r\n"
        f"a=rtpmap:{codec} {codec_name}/8000\r\n"
        f"a=rtpmap:{event_pt} telephone-event/8000\r\n"
        f"a=fmtp:{event_pt} 0-15\r\n"
        "a=ptime:20\r\n"
        "a=sendrecv\r\n"
    )


def response_for(
    request: SIPMessage,
    code: int,
    reason: str,
    *,
    to_tag: str = "",
    body: str = "",
    extra: Iterable[tuple[str, str]] = (),
) -> bytes:
    to = request.header("to")
    if to_tag:
        to = add_tag(to, to_tag)
    fields = [
        ("Via", request.header("via")),
        ("From", request.header("from")),
        ("To", to),
        ("Call-ID", request.header("call-id")),
        ("CSeq", request.header("cseq")),
        ("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO, REGISTER"),
    ]
    fields.extend(extra)
    if body:
        fields.extend([("Content-Type", "application/sdp"), ("Content-Length", str(len(body.encode("latin-1"))))])
    else:
        fields.append(("Content-Length", "0"))
    return sip_bytes(f"SIP/2.0 {code} {reason}", fields, body)


def sip_bytes(start: str, fields: Iterable[tuple[str, str]], body: str = "") -> bytes:
    lines = [start]
    lines.extend(f"{key}: {value}" for key, value in fields if value != "")
    lines.append("")
    lines.append(body)
    return "\r\n".join(lines).encode("latin-1")


def rtp_packet(payload_type: int, sequence: int, timestamp: int, ssrc: int, payload: bytes, marker: bool = False) -> bytes:
    first = 0x80
    second = (0x80 if marker else 0) | (payload_type & 0x7F)
    return struct.pack("!BBHII", first, second, sequence & 0xFFFF, timestamp & 0xFFFFFFFF, ssrc) + payload


def ulaw_sample(sample: int) -> int:
    # ITU-T G.711 mu-law, sufficient for deterministic PCMU test tones.
    sample = max(-32768, min(32767, sample))
    sign = 0x80 if sample < 0 else 0
    magnitude = min(32635, abs(sample)) + 132
    exponent = 7
    mask = 0x4000
    while exponent > 0 and not (magnitude & mask):
        exponent -= 1
        mask >>= 1
    mantissa = (magnitude >> (exponent + 3)) & 0x0F
    return (~(sign | (exponent << 4) | mantissa)) & 0xFF


def tone_payload(phase: int, amplitude: int = 7000) -> bytes:
    values = []
    for offset in range(RTP_SAMPLES):
        # Two harmonics make it obvious that a real non-silent payload crossed
        # the boundary without requiring an audio decoder in the test.
        position = phase + offset
        sample = int(amplitude * (0.7 * math.sin(2 * math.pi * 440 * position / RTP_CLOCK)))
        values.append(ulaw_sample(sample))
    return bytes(values)


@dataclass
class Call:
    call_id: str
    direction: str
    from_header: str
    to_header: str
    remote_target: tuple[str, int] | None = None
    codec: int = PCMU
    codec_name: str = "PCMU"
    event_pt: int = TELEPHONE_EVENT
    remote_rtp: tuple[str, int] | None = None
    invite: SIPMessage | None = None
    invite_addr: tuple[str, int] | None = None
    local_tag: str = field(default_factory=lambda: uuid.uuid4().hex[:12])
    remote_tag: str = ""
    cseq: int = 1
    established: threading.Event = field(default_factory=threading.Event)
    closed: threading.Event = field(default_factory=threading.Event)
    rtp_received: int = 0
    rtp_non_silence: int = 0
    rtp_sequence: int = field(default_factory=lambda: random.randrange(1, 60000))
    rtp_timestamp: int = field(default_factory=lambda: random.randrange(1, 0xFFFF))
    rtp_ssrc: int = field(default_factory=lambda: random.randrange(1, 0xFFFFFFFF))
    media_thread: threading.Thread | None = None


class SIPPeer:
    def __init__(self, port: int, username: str = "voxmail", password: str = "secret"):
        self.port = port
        self.username = username
        self.password = password
        self.realm = "voxmail-local"
        self.nonce = uuid.uuid4().hex
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.bind(("127.0.0.1", port))
        self.rtp_sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.rtp_sock.bind(("127.0.0.1", 0))
        self.rtp_port = self.rtp_sock.getsockname()[1]
        self.running = threading.Event()
        self.registered = threading.Event()
        self.events: queue.Queue[tuple[str, object]] = queue.Queue()
        self.calls: dict[str, Call] = {}
        self.lock = threading.Lock()
        self.thread: threading.Thread | None = None

    def start(self) -> None:
        self.running.set()
        self.thread = threading.Thread(target=self._run, name="voxmail-sip-peer", daemon=True)
        self.thread.start()

    def close(self) -> None:
        self.running.clear()
        for call in list(self.calls.values()):
            call.closed.set()
        if self.thread:
            self.thread.join(timeout=2)
        self.sock.close()
        self.rtp_sock.close()

    def _run(self) -> None:
        while self.running.is_set():
            readable, _, _ = select.select([self.sock, self.rtp_sock], [], [], SIP_TIMEOUT)
            for item in readable:
                if item is self.sock:
                    try:
                        data, address = self.sock.recvfrom(65535)
                    except OSError:
                        continue
                    try:
                        message = parse_sip(data)
                        self._handle_sip(message, address)
                    except Exception as exc:  # surfaced through the driver
                        self.events.put(("peer_error", str(exc)))
                else:
                    try:
                        data, address = self.rtp_sock.recvfrom(4096)
                    except OSError:
                        continue
                    self._handle_rtp(data, address)

    def _send(self, data: bytes, address: tuple[str, int]) -> None:
        self.sock.sendto(data, address)

    def _digest_valid(self, request: SIPMessage) -> bool:
        raw = request.header("authorization")
        if not raw.lower().startswith("digest "):
            return False
        values = parse_header_values(raw[7:])
        if values.get("username") != self.username or values.get("realm") != self.realm:
            return False
        if values.get("nonce") != self.nonce or not values.get("response"):
            return False
        uri = values.get("uri", "")
        ha1 = md5(f"{self.username}:{self.realm}:{self.password}")
        ha2 = md5(f"{request.method}:{uri}")
        qop = values.get("qop", "")
        if qop:
            expected = md5(
                f"{ha1}:{self.nonce}:{values.get('nc', '')}:{values.get('cnonce', '')}:{qop}:{ha2}"
            )
        else:
            expected = md5(f"{ha1}:{self.nonce}:{ha2}")
        return expected.lower() == values.get("response", "").lower()

    def _handle_sip(self, request: SIPMessage, address: tuple[str, int]) -> None:
        if request.status is not None:
            self._handle_response(request, address)
            return
        method = request.method.upper()
        if method == "REGISTER":
            if not self._digest_valid(request):
                challenge = f'Digest realm="{self.realm}", nonce="{self.nonce}", algorithm=MD5, qop="auth"'
                self._send(
                    response_for(request, 401, "Unauthorized", extra=[("WWW-Authenticate", challenge)]),
                    address,
                )
                return
            self.registered.set()
            self.events.put(("registered", request.header("call-id")))
            self._send(
                response_for(
                    request,
                    200,
                    "OK",
                    extra=[("Contact", f"<sip:{self.username}@127.0.0.1:{self.port}>"), ("Expires", "300")],
                ),
                address,
            )
            return
        if method == "OPTIONS":
            self._send(response_for(request, 200, "OK"), address)
            return
        if method == "INVITE":
            self._handle_invite(request, address)
            return
        if method == "ACK":
            call = self.calls.get(request.header("call-id"))
            if call:
                call.established.set()
                self.events.put(("established", call))
            return
        if method == "BYE":
            call = self.calls.get(request.header("call-id"))
            self._send(response_for(request, 200, "OK", to_tag=call.local_tag if call else ""), address)
            if call:
                call.closed.set()
                self.events.put(("closed", call))
            return
        if method == "CANCEL":
            self._send(response_for(request, 200, "OK"), address)
            self._send(response_for(request, 487, "Request Terminated"), address)
            return
        if method == "INFO":
            self._send(response_for(request, 200, "OK"), address)
            self.events.put(("info", request.body))
            return
        self._send(response_for(request, 405, "Method Not Allowed"), address)

    def _handle_invite(self, request: SIPMessage, address: tuple[str, int]) -> None:
        call_id = request.header("call-id")
        remote_ip, remote_port, payloads, event_pt = parse_rtp_target(request.body)
        codec, codec_name = choose_codec(payloads)
        call = Call(
            call_id=call_id,
            direction="outbound",
            from_header=request.header("from"),
            to_header=request.header("to"),
            remote_target=address,
            codec=codec,
            codec_name=codec_name,
            event_pt=event_pt or TELEPHONE_EVENT,
            remote_rtp=(remote_ip, remote_port),
            invite=request,
            invite_addr=address,
            remote_tag=header_tag(request.header("from")),
        )
        with self.lock:
            self.calls[call_id] = call
        self._send(response_for(request, 100, "Trying"), address)
        self._send(response_for(request, 180, "Ringing", to_tag=call.local_tag), address)
        body = make_sdp(self.rtp_port, codec, codec_name, event_pt or TELEPHONE_EVENT)
        self._send(
            response_for(
                request,
                200,
                "OK",
                to_tag=call.local_tag,
                body=body,
                extra=[("Contact", f"<sip:{self.username}@127.0.0.1:{self.port}>")],
            ),
            address,
        )
        self.events.put(("outbound_invite", call))
        self._start_media(call)

    def _handle_response(self, response: SIPMessage, address: tuple[str, int]) -> None:
        cseq = response.header("cseq")
        call = self.calls.get(response.header("call-id"))
        if not call or not cseq:
            return
        method = cseq.split()[1] if len(cseq.split()) > 1 else ""
        if method != "INVITE" or response.status != 200:
            return
        call.remote_tag = header_tag(response.header("to"))
        remote_ip, remote_port, payloads, event_pt = parse_rtp_target(response.body)
        call.codec, call.codec_name = choose_codec(payloads)
        call.event_pt = event_pt or TELEPHONE_EVENT
        call.remote_rtp = (remote_ip, remote_port)
        ack_uri = response.header("contact")
        if ack_uri.startswith("<") and ack_uri.endswith(">"):
            ack_uri = ack_uri[1:-1]
        if not ack_uri:
            ack_uri = f"sip:{self.username}@127.0.0.1:{address[1]}"
        invite = call.invite
        if not invite:
            raise SmokeFailure("inbound call has no original INVITE")
        fields = [
            ("Via", f"SIP/2.0/UDP 127.0.0.1:{self.port};branch=z9hG4bK{uuid.uuid4().hex}"),
            ("From", invite.header("from")),
            ("To", response.header("to")),
            ("Call-ID", call.call_id),
            ("CSeq", f"{call.cseq} ACK"),
            ("Contact", f"<sip:{self.username}@127.0.0.1:{self.port}>"),
            ("Content-Length", "0"),
        ]
        self._send(sip_bytes(f"ACK {ack_uri} SIP/2.0", fields), address)
        call.established.set()
        self.events.put(("established", call))
        self._start_media(call)

    def _handle_rtp(self, data: bytes, address: tuple[str, int]) -> None:
        if len(data) < 12 or (data[0] >> 6) != 2:
            return
        payload_type = data[1] & 0x7F
        with self.lock:
            calls = list(self.calls.values())
        for call in calls:
            if call.closed.is_set() or not call.remote_rtp:
                continue
            # Baresip may select a different source port than the SDP port;
            # the active dialog and negotiated payload type are the identity.
            if payload_type == call.event_pt:
                self.events.put(("remote_dtmf", data[12:]))
            elif payload_type in (call.codec, PCMU, PCMA):
                call.rtp_received += 1
                if any(byte not in (0xFF, 0xD5) for byte in data[12:]):
                    call.rtp_non_silence += 1

    def _start_media(self, call: Call) -> None:
        if call.media_thread:
            return
        call.media_thread = threading.Thread(target=self._media_loop, args=(call,), daemon=True)
        call.media_thread.start()

    def _media_loop(self, call: Call) -> None:
        phase = 0
        while self.running.is_set() and not call.closed.is_set():
            if call.remote_rtp:
                payload = tone_payload(phase)
                packet = rtp_packet(call.codec, call.rtp_sequence, call.rtp_timestamp, call.rtp_ssrc, payload)
                try:
                    self.rtp_sock.sendto(packet, call.remote_rtp)
                except OSError:
                    return
                call.rtp_sequence = (call.rtp_sequence + 1) & 0xFFFF
                call.rtp_timestamp = (call.rtp_timestamp + RTP_SAMPLES) & 0xFFFFFFFF
                phase += RTP_SAMPLES
            time.sleep(RTP_PTIME)

    def _new_inbound_call(self, local_port: int) -> Call:
        call_id = f"voxmail-peer-{uuid.uuid4().hex}"
        from_header = f"<sip:{self.username}@127.0.0.1:{self.port}>;tag={uuid.uuid4().hex[:12]}"
        to_header = f"<sip:voxmail@127.0.0.1:{local_port}>"
        body = make_sdp(self.rtp_port, PCMU, "PCMU", TELEPHONE_EVENT)
        call = Call(
            call_id=call_id,
            direction="inbound",
            from_header=from_header,
            to_header=to_header,
            remote_target=("127.0.0.1", local_port),
            codec=PCMU,
            codec_name="PCMU",
            event_pt=TELEPHONE_EVENT,
            remote_rtp=None,
            invite=None,
            cseq=1,
        )
        with self.lock:
            self.calls[call_id] = call
        fields = [
            ("Via", f"SIP/2.0/UDP 127.0.0.1:{self.port};branch=z9hG4bK{uuid.uuid4().hex}"),
            ("Max-Forwards", "70"),
            ("From", from_header),
            ("To", to_header),
            ("Call-ID", call_id),
            ("CSeq", "1 INVITE"),
            ("Contact", f"<sip:{self.username}@127.0.0.1:{self.port}>"),
            ("Supported", "replaces,100rel"),
            ("Content-Type", "application/sdp"),
            ("Content-Length", str(len(body.encode("latin-1")))),
        ]
        request_bytes = sip_bytes(f"INVITE sip:voxmail@127.0.0.1:{local_port} SIP/2.0", fields, body)
        call.invite = parse_sip(request_bytes)
        self._send(request_bytes, ("127.0.0.1", local_port))
        self.events.put(("inbound_invite", call))
        return call

    def invite(self, local_port: int) -> Call:
        return self._new_inbound_call(local_port)

    def send_bye(self, call: Call) -> None:
        if call.closed.is_set():
            return
        target = call.remote_target
        if not target:
            raise SmokeFailure("cannot send BYE without a dialog target")
        fields = [
            ("Via", f"SIP/2.0/UDP 127.0.0.1:{self.port};branch=z9hG4bK{uuid.uuid4().hex}"),
            ("Max-Forwards", "70"),
            ("From", call.from_header),
            ("To", add_tag(call.to_header, call.remote_tag) if call.remote_tag else call.to_header),
            ("Call-ID", call.call_id),
            ("CSeq", str(call.cseq + 1) + " BYE"),
            ("Content-Length", "0"),
        ]
        self._send(sip_bytes(f"BYE sip:voxmail@127.0.0.1:{target[1]} SIP/2.0", fields), target)

    def send_dtmf(self, call: Call, digit: str = "5") -> None:
        if not call.remote_rtp:
            raise SmokeFailure("cannot send DTMF without negotiated RTP")
        if digit not in "0123456789*#ABCD":
            raise SmokeFailure(f"unsupported DTMF digit {digit!r}")
        event = "0123456789*#ABCD".index(digit)
        timestamp = call.rtp_timestamp
        duration = RTP_SAMPLES
        for final in (False, False, False, True, True):
            duration = min(duration + RTP_SAMPLES, RTP_SAMPLES * 4)
            payload = struct.pack("!BBH", event, 0x8A if final else 0x0A, duration)
            packet = rtp_packet(call.event_pt, call.rtp_sequence, timestamp, call.rtp_ssrc, payload, marker=duration == RTP_SAMPLES)
            self.rtp_sock.sendto(packet, call.remote_rtp)
            call.rtp_sequence = (call.rtp_sequence + 1) & 0xFFFF
            time.sleep(RTP_PTIME)

    def wait_event(self, name: str, timeout: float = TIMEOUT):
        deadline = now() + timeout
        while now() < deadline:
            remaining = max(0.01, deadline - now())
            try:
                event, value = self.events.get(timeout=min(0.2, remaining))
            except queue.Empty:
                continue
            if event == "peer_error":
                raise SmokeFailure(f"local SIP peer error: {value}")
            if event == name:
                return value
        raise SmokeFailure(f"timed out waiting for local SIP peer event {name}")


class Bridge:
    def __init__(self, path: Path):
        self.path = path
        self.sock: socket.socket | None = None
        self.events: queue.Queue[dict] = queue.Queue()
        self.stash: list[dict] = []
        self.running = threading.Event()
        self.reader: threading.Thread | None = None

    def connect(self) -> None:
        self.close()
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        deadline = now() + TIMEOUT
        while now() < deadline:
            try:
                sock.connect(str(self.path))
                break
            except OSError:
                time.sleep(0.05)
        else:
            sock.close()
            raise SmokeFailure(f"timed out connecting to bridge {self.path}")
        self.sock = sock
        self.running.set()
        self.reader = threading.Thread(target=self._read, name="voxmail-bridge-reader", daemon=True)
        self.reader.start()

    def close(self) -> None:
        self.running.clear()
        if self.sock:
            try:
                self.sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            self.sock.close()
            self.sock = None
        if self.reader:
            self.reader.join(timeout=1)
            self.reader = None

    def _read(self) -> None:
        assert self.sock is not None
        stream = self.sock.makefile("rb")
        try:
            for line in stream:
                if not line:
                    break
                message = json.loads(line)
                if message.get("version") != 2:
                    raise SmokeFailure(f"bridge returned unsupported event: {message}")
                self.events.put(message)
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            if self.running.is_set():
                self.events.put({"type": "bridge_error", "reason": str(exc)})
        finally:
            stream.close()

    def send(self, message: dict) -> None:
        if not self.sock:
            raise SmokeFailure("bridge is not connected")
        frame = dict(message)
        frame["version"] = 2
        self.sock.sendall((json.dumps(frame, separators=(",", ":")) + "\n").encode("utf-8"))

    def wait_for(self, event_type: str, *, call_id: str = "", request_id: str = "", timeout: float = TIMEOUT) -> dict:
        deadline = now() + timeout

        def matches(message: dict) -> bool:
            return (
                message.get("type") == event_type
                and (not call_id or message.get("call_id") == call_id)
                and (not request_id or message.get("request_id") == request_id)
            )

        for index, message in enumerate(self.stash):
            if matches(message):
                return self.stash.pop(index)
        while now() < deadline:
            try:
                message = self.events.get(timeout=min(0.2, max(0.01, deadline - now())))
            except queue.Empty:
                continue
            if message.get("type") == "bridge_error":
                raise SmokeFailure(f"bridge reader failed: {message}")
            if matches(message):
                return message
            self.stash.append(message)
        raise SmokeFailure(f"timed out waiting for bridge event {event_type} call={call_id!r} request={request_id!r}")

    def drain(self, timeout: float = 0.25) -> list[dict]:
        """Return queued events without waiting for a particular transition."""
        messages = list(self.stash)
        self.stash.clear()
        deadline = now() + timeout
        while now() < deadline:
            try:
                message = self.events.get(timeout=min(0.05, max(0.01, deadline - now())))
            except queue.Empty:
                break
            if message.get("type") == "bridge_error":
                raise SmokeFailure(f"bridge reader failed: {message}")
            messages.append(message)
        return messages


class Baresip:
    def __init__(self, binary: str, config_dir: Path, control: Path, audio_dir: Path, log_path: Path):
        self.binary = binary
        self.config_dir = config_dir
        self.control = control
        self.audio_dir = audio_dir
        self.log_path = log_path
        self.process: subprocess.Popen | None = None
        self.log_thread: threading.Thread | None = None
        self.log_lines: queue.Queue[str] = queue.Queue()

    def start(self) -> None:
        self.process = subprocess.Popen(
            [self.binary, "-v", "-s", "-f", str(self.config_dir)],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env={**os.environ, "VOXMAIL_CONTROL_SOCKET": str(self.control), "VOXMAIL_AUDIO_DIR": str(self.audio_dir)},
            text=True,
            bufsize=1,
        )

        def drain() -> None:
            assert self.process and self.process.stdout
            with self.log_path.open("a", encoding="utf-8") as log:
                for line in self.process.stdout:
                    log.write(line)
                    log.flush()
                    self.log_lines.put(line.rstrip())

        self.log_thread = threading.Thread(target=drain, name="baresip-log-reader", daemon=True)
        self.log_thread.start()

        ready = False
        deadline = now() + TIMEOUT
        while now() < deadline:
            if self.process.poll() is not None:
                raise SmokeFailure(f"baresip exited during startup with {self.process.returncode}; log={self.log_path}")
            try:
                line = self.log_lines.get(timeout=0.1)
            except queue.Empty:
                continue
            if "baresip is ready" in line:
                ready = True
                break
        if not ready:
            raise SmokeFailure(f"baresip did not become ready; log={self.log_path}")
        wait_until(lambda: self.control.exists(), TIMEOUT, "baresip control socket")

    def stop(self) -> None:
        process = self.process
        self.process = None
        if not process:
            return
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
        if self.log_thread:
            self.log_thread.join(timeout=1)
            self.log_thread = None


def write_config(
    directory: Path,
    peer_port: int,
    local_port: int,
    rtp_base: int,
    username: str = "voxmail",
    password: str = "secret",
) -> None:
    config = "\n".join(
        [
            "module_path /usr/local/lib/baresip/modules",
            "module_app voxmail.so",
            "module account.so",
            "module g711.so",
            "module auconv.so",
            "module auresamp.so",
            "module aubridge.so",
            "module aufile.so",
            "module in_band_dtmf.so",
            "module ice.so",
            "module stun.so",
            "module srtp.so",
            "module dtls_srtp.so",
            "module stdio.so",
            "call_accept yes",
            "poll_method poll",
            "audio_source voxmail,",
            "audio_player voxmail,",
            "audio_alert voxmail,",
            "ausrc_srate 8000",
            "auplay_srate 8000",
            "ausrc_channels 1",
            "auplay_channels 1",
            "ausrc_format s16",
            "auplay_format s16",
            "audio_telev_pt 101",
            "audio_buffer 60-200",
            f"rtp_ports {rtp_base}-{rtp_base + 100}",
            "call_max_calls 4",
            "net_interface 127.0.0.1",
            f"sip_listen 127.0.0.1:{local_port}",
            "sip_transports udp",
            "",
        ]
    )
    (directory / "config").write_text(config, encoding="utf-8")
    encoded_username = quote(username, safe="-._~")
    # Baresip's auth_* fields are account-file parameters, not URI values:
    # percent escapes are not decoded by its parameter parser.  Quoted
    # parameters preserve reserved characters (including semicolons and
    # spaces) exactly; the pinned parser also preserves embedded quotes and
    # backslashes in the returned value.
    account_username = f'"{username}"'
    account_password = f'"{password}"'
    (directory / "accounts").write_text(
        f"<sip:{encoded_username}@127.0.0.1:{peer_port};transport=udp>;auth_user={account_username};auth_pass={account_password};regint=300\n",
        encoding="utf-8",
    )


def open_tx_fifo(path: Path) -> int:
    deadline = now() + TIMEOUT
    while now() < deadline:
        try:
            return os.open(path, os.O_WRONLY | os.O_NONBLOCK)
        except OSError:
            time.sleep(0.05)
    raise SmokeFailure(f"timed out opening Baresip TX FIFO {path}")


def pump_pcm(fd: int, stop: threading.Event) -> None:
    phase = 0
    frame = bytearray()
    while not stop.is_set():
        frame.clear()
        for offset in range(RTP_SAMPLES):
            sample = int(9000 * math.sin(2 * math.pi * 440 * (phase + offset) / RTP_CLOCK))
            frame.extend(struct.pack("<h", sample))
        phase += RTP_SAMPLES
        try:
            view = memoryview(frame)
            try:
                while view and not stop.is_set():
                    try:
                        written = os.write(fd, view)
                    except BlockingIOError:
                        time.sleep(RTP_PTIME / 2)
                        continue
                    view = view[written:]
            finally:
                view.release()
        except OSError:
            return
        time.sleep(RTP_PTIME)


def assert_rx_audio(path: Path) -> None:
    def has_audio() -> bool:
        try:
            data = path.read_bytes()
        except FileNotFoundError:
            return False
        return len(data) >= 640 and any(byte != 0 for byte in data[: min(len(data), 8192)])

    wait_until(has_audio, TIMEOUT, f"non-silent captured PCM in {path}")


def run_call_audio_and_dtmf(peer: SIPPeer, bridge: Bridge, event: dict, label: str) -> str:
    call_id = event.get("call_id", "")
    tx_value = event.get("tx_path", "")
    rx_value = event.get("rx_path", "")
    if not call_id or not isinstance(tx_value, str) or not isinstance(rx_value, str) or not tx_value or not rx_value:
        raise SmokeFailure(f"{label} event omitted media identity: {event}")
    tx_path = Path(tx_value)
    rx_path = Path(rx_value)
    tx_fd = open_tx_fifo(tx_path)
    stop = threading.Event()
    writer = threading.Thread(target=pump_pcm, args=(tx_fd, stop), daemon=True)
    writer.start()
    try:
        wait_until(lambda: peer.calls.get(call_id) is not None and peer.calls[call_id].established.is_set(), TIMEOUT, f"{label} SIP establishment")
        peer_call = peer.calls[call_id]
        wait_until(lambda: peer_call.rtp_non_silence > 0, TIMEOUT, f"{label} outbound RTP audio")
        assert_rx_audio(rx_path)
        peer.send_dtmf(peer_call, "5")
        dtmf = bridge.wait_for("dtmf", call_id=call_id, timeout=TIMEOUT)
        if dtmf.get("digit") != "5" or dtmf.get("phase") != "end":
            raise SmokeFailure(f"unexpected {label} DTMF event: {dtmf}")
    finally:
        stop.set()
        try:
            os.close(tx_fd)
        except OSError:
            pass
        writer.join(timeout=1)
    return call_id


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baresip", default="baresip")
    args = parser.parse_args()
    if not shutil.which(args.baresip) and not Path(args.baresip).is_file():
        raise SmokeFailure(f"Baresip binary not found: {args.baresip}")

    with tempfile.TemporaryDirectory(prefix="voxmail-sip-peer-") as raw_dir:
        root = Path(raw_dir)
        config_dir = root / "config"
        audio_dir = root / "audio"
        config_dir.mkdir(mode=0o700)
        audio_dir.mkdir(mode=0o700)
        peer_port = free_udp_port()
        local_port = free_udp_port()
        rtp_base = max(20000, free_udp_port() // 100 * 100)
        username = os.environ.get("VOXMAIL_SIP_PEER_USERNAME", "voxmail")
        password = os.environ.get("VOXMAIL_SIP_PEER_PASSWORD", "secret")
        if not username or not password:
            raise SmokeFailure("SIP peer username and password must be non-empty")
        write_config(config_dir, peer_port, local_port, rtp_base, username, password)
        peer = SIPPeer(peer_port, username, password)
        peer.start()
        baresip = Baresip(args.baresip, config_dir, root / "baresip.sock", audio_dir, root / "baresip.log")
        bridge = Bridge(root / "baresip.sock")
        summary = {"registration": False, "outbound": False, "inbound": False, "restart": False}
        try:
            baresip.start()
            peer.wait_event("registered")
            # Deliberately connect after the SIP registration transition. The
            # native event queue must retain that event until the bridge exists.
            time.sleep(0.05)
            bridge.connect()
            bridge.wait_for("registration", timeout=TIMEOUT)
            summary["registration"] = True

            request_id = "sip-peer-outbound"
            bridge.send({"type": "dial", "request_id": request_id, "to": f"sip:peer@127.0.0.1:{peer_port}"})
            outgoing = bridge.wait_for("call_outgoing", request_id=request_id)
            call_id = run_call_audio_and_dtmf(peer, bridge, outgoing, "outbound")
            bridge.send({"type": "hangup", "call_id": call_id, "code": 603, "reason": "local SIP smoke"})
            bridge.wait_for("call_closed", call_id=call_id)
            bridge.send({"type": "hangup", "call_id": call_id, "code": 603, "reason": "repeat"})
            duplicate = bridge.wait_for("command_error", call_id=call_id)
            if duplicate.get("code") != 481:
                raise SmokeFailure(f"repeated hangup did not return 481: {duplicate}")
            summary["outbound"] = True

            bridge.close()
            bridge.connect()
            # Registration notifications describe transitions; reconnecting the
            # event stream does not replay the already-consumed registered state.
            # The inbound call below proves that the replacement bridge is live.

            inbound_peer_call = peer.invite(local_port)
            incoming = bridge.wait_for("call_incoming")
            if not incoming.get("call_id"):
                raise SmokeFailure(f"incoming event omitted call ID: {incoming}")
            if incoming["call_id"] != inbound_peer_call.call_id:
                raise SmokeFailure("incoming event call ID did not match SIP dialog")
            bridge.send({"type": "answer", "call_id": incoming["call_id"]})
            bridge.wait_for("call_established", call_id=incoming["call_id"])
            run_call_audio_and_dtmf(peer, bridge, incoming, "inbound")
            # Deliver a remote BYE and a local hangup back-to-back. Whichever
            # reaches the Baresip loop first must win exactly once; the other
            # command may be a harmless stale 481, but it must not duplicate
            # the call_closed event or crash the bridge.
            peer.send_bye(inbound_peer_call)
            bridge.send({"type": "hangup", "call_id": inbound_peer_call.call_id, "code": 603, "reason": "close race"})
            bridge.wait_for("call_closed", call_id=inbound_peer_call.call_id)
            if any(
                message.get("type") == "call_closed" and message.get("call_id") == inbound_peer_call.call_id
                for message in bridge.drain()
            ):
                raise SmokeFailure("close race emitted duplicate call_closed event")
            summary["inbound"] = True

            restart_peer_call = peer.invite(local_port)
            restart_event = bridge.wait_for("call_incoming")
            bridge.send({"type": "answer", "call_id": restart_event["call_id"]})
            bridge.wait_for("call_established", call_id=restart_event["call_id"])
            del restart_peer_call
            bridge.close()
            baresip.stop()
            # The pinned Baresip process can leave a stale AF_UNIX pathname
            # after SIGTERM; it must not leave a live listener.  The next
            # startup removes the stale pathname before binding.
            wait_until(lambda: control_socket_is_dead(root / "baresip.sock"), TIMEOUT, "control socket shutdown on restart")
            baresip.start()
            bridge.connect()
            peer.wait_event("registered")
            bridge.wait_for("registration", timeout=TIMEOUT)
            summary["restart"] = True
        except (SmokeFailure, OSError, subprocess.SubprocessError) as exc:
            try:
                log_tail = (root / "baresip.log").read_text(encoding="utf-8", errors="replace").splitlines()[-80:]
            except OSError:
                log_tail = []
            detail = str(exc)
            if log_tail:
                detail += "\nBaresip log tail:\n" + "\n".join(log_tail)
            raise SmokeFailure(detail) from exc
        finally:
            bridge.close()
            baresip.stop()
            peer.close()
        print(json.dumps(summary, sort_keys=True))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (SmokeFailure, OSError, subprocess.SubprocessError) as exc:
        print(f"sip-peer-smoke failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
