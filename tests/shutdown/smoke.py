#!/usr/bin/env python3
"""Stop an exact local image during SIP, scheduled sync, and voice download.

All credentials and services are disposable fixtures. An internal Docker
network has no external route; its public-looking subnet exercises the normal
outbound policy without adding an application policy bypass. The model host
is mapped to a local HTTPS stall fixture using a test-only CA. The application,
Baresip, mbsync, and voice job/cancellation paths are the actual image binaries.
"""

from __future__ import annotations

import argparse
import hashlib
import http.cookiejar
import http.server
import json
import os
from pathlib import Path
import socket
import sqlite3
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid


def wait_for(check, seconds: float, description: str):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.1)
    raise RuntimeError(f"timed out waiting for {description}")


def workloads() -> None:
    sys.path.insert(0, "/fixture")
    from tests.sippeer.smoke import SIPPeer, response_for

    class RegistrationPeer(SIPPeer):
        reject_registration = False

        def _handle_sip(self, message, address):
            if message.status is None and message.method.upper() == "REGISTER" and self.reject_registration:
                self._send(response_for(message, 403, "Forbidden"), address)
                return
            super()._handle_sip(message, address)

    proof = Path("/proof")
    db_path = Path("/data/sqlite/voxmail.db")
    csrf = ""
    client = urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
    )

    def request(path: str, body=None, expected=200, timeout=10):
        nonlocal csrf
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request("http://127.0.0.1:8080" + path, data=data)
        req.add_header("Content-Type", "application/json")
        if csrf:
            req.add_header("X-CSRF-Token", csrf)
        if path == "/api/v1/sip" and body is not None:
            req.method = "PUT"
        try:
            response = client.open(req, timeout=timeout)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            raw = response.read()
            if response.status != expected:
                raise RuntimeError(f"{path}: expected {expected}, got {response.status}: {raw!r}")
            csrf = response.headers.get("X-CSRF-Token", csrf)
            return json.loads(raw) if raw else None

    def healthy():
        try:
            return request("/healthz")
        except (OSError, RuntimeError):
            return None

    def stalled_imap(port, marker):
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(("0.0.0.0", port))
            listener.listen()
            while True:
                conn, _ = listener.accept()

                def stall(conn=conn):
                    with conn:
                        # Preflight dials then closes; the real mbsync TLS
                        # client sends a ClientHello and blocks for a reply.
                        if conn.recv(4096):
                            (proof / marker).touch()
                            while conn.recv(4096):
                                pass

                threading.Thread(target=stall, daemon=True).start()

    class ModelHandler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Length", str(64 << 20))
            self.end_headers()
            self.wfile.write(b"fixture model download\n")
            self.wfile.flush()
            (proof / "voice-blocked").touch()
            # Wait until the application's canceled request closes its socket.
            try:
                self.connection.recv(1)
            except OSError:
                pass
            (proof / "voice-disconnected").touch()

        def log_message(self, *args):
            pass

    threading.Thread(target=stalled_imap, args=(1993, "sync-blocked"), daemon=True).start()
    threading.Thread(target=stalled_imap, args=(1994, "manual-sync-blocked"), daemon=True).start()
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(proof / "model.pem", proof / "model.key")
    model = http.server.ThreadingHTTPServer(("0.0.0.0", 443), ModelHandler)
    model.socket = tls.wrap_socket(model.socket, server_side=True)
    threading.Thread(target=model.serve_forever, daemon=True).start()
    peer = RegistrationPeer(15061, host="8.8.0.2")
    peer.start()
    try:
        wait_for(healthy, 45, "application health")
        user = request("/api/v1/setup", {"username": "shutdown-admin", "password": "shutdown-fixture-password", "pin": "123456"}, 201)
        request("/api/v1/whitelist", {"phone": "voxmail"}, 201)
        account = request("/api/v1/accounts", {
            "canonical_name": "shutdown mailbox", "email": "shutdown@example.com",
            "imap_host": "8.8.0.2", "imap_port": 1993, "imap_user": "fixture",
            "imap_password": "fixture-password", "imap_security": "implicit_tls",
            "smtp_host": "8.8.0.2", "smtp_port": 1465, "smtp_user": "fixture",
            "smtp_password": "fixture-password", "smtp_security": "implicit_tls",
            "sync_interval_minutes": 1, "folder_map": {"INBOX": "Inbox"},
        }, 201)
        account_id = account["id"]
        # Also retain representative mail, attachment, and recording artifacts.
        mail = Path("/data/mail") / account_id / "Inbox" / "cur"
        mail.mkdir(parents=True, exist_ok=True)
        fixture_mail = mail / "fixture.eml"
        fixture_mail.write_text("From: fixture@example.com\r\nSubject: Shutdown fixture\r\n\r\nRetained mail.\r\n")
        fixture_recording = Path("/data/recordings") / "fixture.wav"
        fixture_recording.write_bytes(b"retained recording fixture")
        (proof / "waiting-for-sync").touch()
        wait_for(lambda: (proof / "sync-blocked").exists(), 100, "scheduled real mbsync TLS operation")
        with sqlite3.connect(db_path) as db:
            row = db.execute("SELECT id FROM sync_runs WHERE account_id=? AND finished_at IS NULL", (account_id,)).fetchone()
        if row is None:
            raise RuntimeError("scheduled synchronization is not durably active")
        sync_id = row[0]
        # Create this second account after the scheduled worker is blocked, so
        # its active operation belongs to an HTTP request rather than Run's
        # background context. Shutdown must cancel and join both scopes.
        manual = request("/api/v1/accounts", {
            "canonical_name": "manual shutdown mailbox", "email": "manual@example.com",
            "imap_host": "8.8.0.2", "imap_port": 1994, "imap_user": "fixture",
            "imap_password": "fixture-password", "imap_security": "implicit_tls",
            "smtp_host": "8.8.0.2", "smtp_port": 1465, "smtp_user": "fixture",
            "smtp_password": "fixture-password", "smtp_security": "implicit_tls",
            "sync_interval_minutes": 60, "folder_map": {"INBOX": "Inbox"},
        }, 201)
        def manual_sync():
            try:
                request("/api/v1/accounts/" + manual["id"] + "/sync", {}, 502, timeout=120)
            except (OSError, RuntimeError):
                pass # HTTP teardown can close the response after cancellation.
            finally:
                (proof / "manual-request-finished").touch()
        threading.Thread(target=manual_sync, daemon=True).start()
        wait_for(lambda: (proof / "manual-sync-blocked").exists(), 15, "manual real mbsync TLS operation")
        with sqlite3.connect(db_path) as db:
            row = db.execute("SELECT id FROM sync_runs WHERE account_id=? AND finished_at IS NULL", (manual["id"],)).fetchone()
        if row is None or (proof / "manual-request-finished").exists():
            raise RuntimeError("manual HTTP synchronization is not durably active")
        manual_sync_id = row[0]
        accounts = request("/api/v1/accounts")
        running = {item["id"]: item.get("last_sync", {}) for item in accounts}
        for identity in (account_id, manual["id"]):
            if running[identity].get("state") != "running" or running[identity].get("finished_at"):
                raise RuntimeError("console API did not report the actual blocked mbsync worker")
        # Exercise the production native registration -> Go runtime -> web
        # snapshot boundary, including a terminal provider rejection and retry.
        peer.wait_event("registered", timeout=15)
        sip_settings = {
            "domain": "8.8.0.2", "username": "voxmail", "password": "secret",
            "enabled": True, "local_port": 15060, "registrar_port": 15061,
            "transport": "udp", "reg_interval": 10,
        }

        def registration_snapshot(wanted):
            state = request("/api/v1/status")
            return state if state.get("runtime", {}).get("registration") == wanted else None

        wait_for(lambda: registration_snapshot("registered"), 15, "console registered SIP snapshot")
        peer.reject_registration = True
        request("/api/v1/sip", sip_settings)
        failure = wait_for(lambda: registration_snapshot("failed"), 15, "console failed SIP snapshot")
        if failure.get("sip_failure", {}).get("diagnostic") != "SIP response 403" or not failure["sip_failure"].get("action"):
            raise RuntimeError("console SIP rejection lacks its actual response and recovery action")
        peer.reject_registration = False
        request("/api/v1/sip", sip_settings)
        recovered = wait_for(lambda: registration_snapshot("registered"), 15, "console SIP registration recovery")
        if "sip_failure" in recovered:
            raise RuntimeError("successful registration retained a stale failure")
        job = request("/api/v1/voices/install", {"voice": "en_US-hfc_male-medium"}, 202)
        wait_for(lambda: (proof / "voice-blocked").exists(), 15, "real voice download")
        call = peer.invite(15060)
        wait_for(call.established.is_set, 15, "actual SIP call establishment")
        # The server's own call admission gate must see the established session.
        request("/api/v1/sip", {
            "domain": "127.0.0.1", "username": "voxmail", "password": "secret",
            "enabled": True, "local_port": 15060, "registrar_port": 15061,
            "transport": "udp", "reg_interval": 10,
        }, 409)
        status = request("/api/v1/voices/jobs/" + job["job_id"])
        if status["status"] != "running":
            raise RuntimeError(f"voice installation is not active: {status}")
        (proof / "ready.json").write_text(json.dumps({
            "user_id": user["id"], "account_id": account_id,
            "sync_id": sync_id, "manual_sync_id": manual_sync_id, "job_id": job["job_id"], "call_id": call.call_id,
            "mail_path": str(fixture_mail), "recording_path": str(fixture_recording),
            "operational_status": True,
        }))
        # Observe call termination before Docker removes fixture processes.
        call.closed.wait(60)
        if call.closed.is_set():
            (proof / "call-closed").touch()
        while True:
            time.sleep(1)
    finally:
        peer.close()


def docker(*args: str, timeout=120) -> str:
    result = subprocess.run(["docker", *args], check=True, text=True, capture_output=True, timeout=timeout)
    return result.stdout.strip()


def verify_container(image_id: str, volume: str, proof: Path) -> dict:
    args = [
        "docker", "run", "--rm", "--mount", f"type=volume,source={volume},target=/data,readonly",
        "--mount", f"type=bind,source={proof},target=/proof,readonly",
        "--mount", f"type=bind,source={Path(__file__).resolve().parents[2]},target=/fixture,readonly",
        "--entrypoint", "python3", image_id, "/fixture/tests/shutdown/verify.py",
    ]
    result = subprocess.run(args, text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError(f"post-stop verification failed: {result.stdout}{result.stderr}")
    return json.loads(result.stdout.strip())


def host(image: str, binary: Path | None = None) -> None:
    if binary is not None:
        binary = binary.resolve()
        if not binary.stat().st_mode & 0o001:
            raise RuntimeError("mounted binary must be executable by the container user")
    image_id = docker("image", "inspect", "--format", "{{.Id}}", image)
    repo = Path(__file__).resolve().parents[2]
    run_id = uuid.uuid4().hex[:12]
    name = "voxmail-shutdown-" + run_id
    volume = name + "-data"
    with tempfile.TemporaryDirectory(prefix="voxmail-shutdown-") as raw:
        root = Path(raw)
        root.chmod(0o755)
        proof = root / "proof"
        proof.mkdir(mode=0o777)
        proof.chmod(0o777)
        subprocess.run([
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-subj", "/CN=huggingface.co", "-addext", "subjectAltName=DNS:huggingface.co",
            "-keyout", str(proof / "model.key"), "-out", str(proof / "model.pem"),
        ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        (proof / "model.pem").chmod(0o644)
        (proof / "model.key").chmod(0o644)
        network_created = False
        volume_created = False
        try:
            docker("network", "create", "--internal", "--subnet", "8.8.0.0/24", name)
            network_created = True
            docker("volume", "create", volume)
            volume_created = True
            docker("run", "-d", "--name", name, "--network", name, "--ip", "8.8.0.2",
                   "--add-host", "huggingface.co:8.8.0.2",
                   "--mount", f"type=bind,source={repo},target=/fixture,readonly",
                   "--mount", f"type=bind,source={proof},target=/proof",
                   "--mount", f"type=volume,source={volume},target=/data",
                   "--env", "VOXMAIL_ENCRYPTION_KEY=shutdown-fixture-key-32-bytes-long",
                   "--env", "SSL_CERT_FILE=/proof/model.pem",
                   "--env", "VOXMAIL_BARESIP_BINARY=/fixture/tests/shutdown/baresip.sh",
                   "--env", "VOXMAIL_SIP_LOCAL_PORT=15060",
                   "--env", 'VOXMAIL_SIP_ACCOUNT=<sip:voxmail@8.8.0.2:15060>;auth_user=voxmail;auth_pass=secret;outbound="sip:8.8.0.2:15061";regint=10',
                   *([] if binary is None else ["--mount", f"type=bind,source={binary},target=/usr/local/bin/voxmail,readonly"]),
                   image_id)
            docker("exec", "--user", "0", "-d", name, "python3", "/fixture/tests/shutdown/smoke.py", "--workloads")

            def ready():
                if (proof / "failed.txt").exists():
                    raise RuntimeError((proof / "failed.txt").read_text())
                return (proof / "ready.json").exists()

            print("Waiting for scheduled synchronization, voice download, and established SIP call", flush=True)
            wait_for(ready, 160, "all active workloads")
            process_table = docker("top", name, "-eo", "pid,comm,args")
            if not any("/usr/local/bin/voxmail" in line and "voxmail-entrypoint" not in line for line in process_table.splitlines()[1:]):
                raise RuntimeError(f"entrypoint did not replace itself with VOXMail:\n{process_table}")
            active = json.loads((proof / "ready.json").read_text())
            started = time.monotonic()
            docker("stop", "--time", "60", name, timeout=70)
            elapsed = time.monotonic() - started
            state = json.loads(docker("inspect", "--format", "{{json .State}}", name))
            if state["ExitCode"] != 0 or state["OOMKilled"]:
                raise RuntimeError(f"application failed graceful shutdown: {state}")
            verified = verify_container(image_id, volume, proof)
            identity = {} if binary is None else {"application_sha256": hashlib.sha256(binary.read_bytes()).hexdigest()}
            print(json.dumps({"image": image_id, **identity, "shutdown_seconds": round(elapsed, 3), "operational_status": active.get("operational_status", False), **verified}, sort_keys=True), flush=True)
        except Exception:
            try:
                print(docker("logs", "--tail", "60", name), file=sys.stderr)
            except Exception:
                pass
            try:
                print(docker("exec", name, "sh", "-c", "for f in /data/logs/baresip.log /data/config/baresip/config /data/config/baresip/accounts; do if [ -f \"$f\" ]; then echo --- $f; tail -80 \"$f\"; fi; done; ls -la /data/run"), file=sys.stderr)
            except Exception:
                pass
            raise
        finally:
            try:
                docker("rm", "-f", name)
            finally:
                try:
                    if volume_created:
                        docker("volume", "rm", volume)
                finally:
                    if network_created:
                        docker("network", "rm", name)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image", nargs="?")
    parser.add_argument("--workloads", action="store_true")
    parser.add_argument("--binary", type=Path, help="current application executable, mounted read-only")
    args = parser.parse_args()
    if args.workloads:
        try:
            workloads()
        except Exception as exc:
            Path("/proof/failed.txt").write_text(str(exc))
            raise
    elif args.image:
        host(args.image, args.binary)
    else:
        parser.error("provide an image to test")
