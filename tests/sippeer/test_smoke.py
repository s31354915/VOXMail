import hashlib
import socket
import unittest

try:
    from tests.sippeer.smoke import (
        PCMU,
        TELEPHONE_EVENT,
        SIPPeer,
        choose_codec,
        free_udp_port,
        make_sdp,
        parse_header_values,
        parse_rtp_target,
        parse_sip,
        response_for,
        sip_bytes,
        ulaw_sample,
        write_config,
    )
except ModuleNotFoundError as exc:
    if exc.name != "tests":
        raise
    from smoke import (
        PCMU,
        TELEPHONE_EVENT,
        SIPPeer,
        choose_codec,
        free_udp_port,
        make_sdp,
        parse_header_values,
        parse_rtp_target,
        parse_sip,
        response_for,
        sip_bytes,
        ulaw_sample,
        write_config,
    )


class SIPPeerContractTests(unittest.TestCase):
    def test_sip_parser_respects_content_length_and_case_insensitive_headers(self):
        message = sip_bytes(
            "REGISTER sip:example.test SIP/2.0",
            [("Call-ID", "call-1"), ("Content-Length", "4")],
            "body-extra",
        )
        parsed = parse_sip(message)
        self.assertEqual(parsed.method, "REGISTER")
        self.assertEqual(parsed.header("call-id"), "call-1")
        self.assertEqual(parsed.body, "body")

    def test_sdp_advertises_codec_and_rfc4733_event(self):
        sdp = make_sdp(30000, PCMU, "PCMU")
        address, port, payloads, event_pt = parse_rtp_target(sdp)
        self.assertEqual(address, "127.0.0.1")
        self.assertEqual(port, 30000)
        self.assertEqual(payloads, [PCMU, TELEPHONE_EVENT])
        self.assertEqual(event_pt, TELEPHONE_EVENT)

    def test_codec_selection_is_deterministic_and_rejects_unsupported_audio(self):
        self.assertEqual(choose_codec([TELEPHONE_EVENT, 8, 0]), (0, "PCMU"))
        self.assertEqual(choose_codec([TELEPHONE_EVENT, 8]), (8, "PCMA"))
        with self.assertRaises(RuntimeError):
            choose_codec([TELEPHONE_EVENT])

    def test_digest_challenge_matches_rfc3261_response(self):
        peer = SIPPeer(0)
        try:
            uri = "sip:127.0.0.1:5060"
            nonce = peer.nonce
            nc = "00000001"
            cnonce = "client-nonce"
            ha1 = hashlib.md5(b"voxmail:voxmail-local:secret", usedforsecurity=False).hexdigest()
            ha2 = hashlib.md5(f"REGISTER:{uri}".encode(), usedforsecurity=False).hexdigest()
            digest = hashlib.md5(
                f"{ha1}:{nonce}:{nc}:{cnonce}:auth:{ha2}".encode(), usedforsecurity=False
            ).hexdigest()
            authorization = (
                'Digest username="voxmail", realm="voxmail-local", '
                f'nonce="{nonce}", uri="{uri}", response="{digest}", '
                'algorithm=MD5, qop=auth, nc=00000001, cnonce="client-nonce"'
            )
            request = parse_sip(
                sip_bytes(
                    "REGISTER sip:127.0.0.1:5060 SIP/2.0",
                    [("Authorization", authorization), ("Content-Length", "0")],
                )
            )
            self.assertTrue(peer._digest_valid(request))
            self.assertFalse(peer._digest_valid(parse_sip(sip_bytes(
                "REGISTER sip:127.0.0.1:5060 SIP/2.0",
                [("Authorization", authorization.replace(digest, "0" * len(digest))), ("Content-Length", "0")],
            ))))
        finally:
            peer.close()

    def test_response_adds_dialog_tag_without_rewriting_existing_tag(self):
        request = parse_sip(
            sip_bytes(
                "INVITE sip:peer@example.test SIP/2.0",
                [("From", "<sip:a@example.test>;tag=from"), ("To", "<sip:b@example.test>"), ("Call-ID", "c"), ("CSeq", "1 INVITE"), ("Via", "SIP/2.0/UDP 127.0.0.1:1"), ("Content-Length", "0")],
            )
        )
        response = response_for(request, 200, "OK", to_tag="to")
        self.assertIn(b"To: <sip:b@example.test>;tag=to", response)
        self.assertIn(b"Content-Length: 0", response)

    def test_ulaw_tone_has_non_silent_samples(self):
        samples = {ulaw_sample(value) for value in (-9000, -1000, 0, 1000, 9000)}
        self.assertGreater(len(samples), 2)

    def test_config_pins_production_native_contract(self):
        from pathlib import Path
        import tempfile

        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            write_config(directory, 5060, 5070, 20000)
            config = (directory / "config").read_text()
            accounts = (directory / "accounts").read_text()
            self.assertIn("module_app voxmail.so", config)
            self.assertIn("audio_telev_pt 101", config)
            self.assertIn("ausrc_format s16", config)
            self.assertIn("sip_listen 127.0.0.1:5070", config)
            self.assertIn("<sip:voxmail@127.0.0.1:5060;transport=udp>", accounts)

    def test_udp_peer_challenges_register_and_answers_outbound_invite(self):
        peer = SIPPeer(free_udp_port())
        client = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        client.bind(("127.0.0.1", 0))
        client.settimeout(2)
        try:
            peer.start()
            client_address = ("127.0.0.1", client.getsockname()[1])
            via = f"SIP/2.0/UDP 127.0.0.1:{client_address[1]};branch=z9hG4bKregister"
            common = [
                ("Via", via),
                ("From", "<sip:voxmail@127.0.0.1>;tag=client"),
                ("To", "<sip:voxmail@127.0.0.1>"),
                ("Call-ID", "register-test"),
                ("CSeq", "1 REGISTER"),
                ("Contact", "<sip:voxmail@127.0.0.1>"),
                ("Content-Length", "0"),
            ]
            client.sendto(sip_bytes(f"REGISTER sip:127.0.0.1:{peer.port} SIP/2.0", common), ("127.0.0.1", peer.port))
            challenge = parse_sip(client.recvfrom(8192)[0])
            self.assertEqual(challenge.status, 401)
            auth = parse_header_values(challenge.header("www-authenticate")[7:])
            uri = f"sip:127.0.0.1:{peer.port}"
            ha1 = hashlib.md5(b"voxmail:voxmail-local:secret", usedforsecurity=False).hexdigest()
            ha2 = hashlib.md5(f"REGISTER:{uri}".encode(), usedforsecurity=False).hexdigest()
            response = hashlib.md5(
                f"{ha1}:{auth['nonce']}:00000001:cnonce:auth:{ha2}".encode(), usedforsecurity=False
            ).hexdigest()
            authorized = common + [
                (
                    "Authorization",
                    f'Digest username="voxmail", realm="voxmail-local", nonce="{auth["nonce"]}", '
                    f'uri="{uri}", response="{response}", algorithm=MD5, qop=auth, '
                    'nc=00000001, cnonce="cnonce"',
                )
            ]
            client.sendto(sip_bytes(f"REGISTER {uri} SIP/2.0", authorized), ("127.0.0.1", peer.port))
            registered = parse_sip(client.recvfrom(8192)[0])
            self.assertEqual(registered.status, 200)
            peer.wait_event("registered")

            rtp_port = free_udp_port()
            call_id = "outbound-dialog-test"
            body = make_sdp(rtp_port, PCMU, "PCMU")
            invite = sip_bytes(
                f"INVITE sip:peer@127.0.0.1:{peer.port} SIP/2.0",
                [
                    ("Via", f"SIP/2.0/UDP 127.0.0.1:{client_address[1]};branch=z9hG4bKinvite"),
                    ("From", "<sip:voxmail@127.0.0.1>;tag=client-call"),
                    ("To", "<sip:peer@127.0.0.1>"),
                    ("Call-ID", call_id),
                    ("CSeq", "1 INVITE"),
                    ("Contact", "<sip:voxmail@127.0.0.1>"),
                    ("Content-Type", "application/sdp"),
                    ("Content-Length", str(len(body.encode("latin-1")))),
                ],
                body,
            )
            client.sendto(invite, ("127.0.0.1", peer.port))
            responses = []
            while not any(response.status == 200 for response in responses):
                responses.append(parse_sip(client.recvfrom(8192)[0]))
            ok = next(response for response in responses if response.status == 200)
            call = peer.wait_event("outbound_invite")
            ack = sip_bytes(
                f"ACK sip:peer@127.0.0.1:{peer.port} SIP/2.0",
                [
                    ("Via", via),
                    ("From", "<sip:voxmail@127.0.0.1>;tag=client-call"),
                    ("To", ok.header("to")),
                    ("Call-ID", call_id),
                    ("CSeq", "1 ACK"),
                    ("Content-Length", "0"),
                ],
            )
            client.sendto(ack, ("127.0.0.1", peer.port))
            peer.wait_event("established")
            self.assertEqual(call.call_id, call_id)

            bye = sip_bytes(
                f"BYE sip:peer@127.0.0.1:{peer.port} SIP/2.0",
                [
                    ("Via", via),
                    ("From", "<sip:voxmail@127.0.0.1>;tag=client-call"),
                    ("To", ok.header("to")),
                    ("Call-ID", call_id),
                    ("CSeq", "2 BYE"),
                    ("Content-Length", "0"),
                ],
            )
            client.sendto(bye, ("127.0.0.1", peer.port))
            self.assertEqual(parse_sip(client.recvfrom(8192)[0]).status, 200)
            peer.wait_event("closed")
        finally:
            peer.close()
            client.close()

    def test_udp_peer_completes_inbound_dialog_and_acknowledges_remote_bye(self):
        peer = SIPPeer(free_udp_port())
        fake = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        fake.bind(("127.0.0.1", 0))
        fake.settimeout(2)
        try:
            peer.start()
            call = peer.invite(fake.getsockname()[1])
            peer.wait_event("inbound_invite")
            invite_data, peer_address = fake.recvfrom(8192)
            invite = parse_sip(invite_data)
            answer_body = make_sdp(free_udp_port(), PCMU, "PCMU")
            response = response_for(invite, 200, "OK", to_tag="fake-uas", body=answer_body)
            fake.sendto(response, peer_address)
            ack_data, _ = fake.recvfrom(8192)
            self.assertEqual(parse_sip(ack_data).method, "ACK")
            peer.wait_event("established")
            self.assertTrue(call.established.is_set())

            bye = sip_bytes(
                "BYE sip:voxmail@127.0.0.1 SIP/2.0",
                [
                    ("Via", f"SIP/2.0/UDP 127.0.0.1:{fake.getsockname()[1]};branch=z9hG4bKbye"),
                    ("From", "<sip:voxmail@127.0.0.1>;tag=fake-uas"),
                    ("To", call.from_header),
                    ("Call-ID", call.call_id),
                    ("CSeq", "2 BYE"),
                    ("Content-Length", "0"),
                ],
            )
            fake.sendto(bye, peer_address)
            self.assertEqual(parse_sip(fake.recvfrom(8192)[0]).status, 200)
            peer.wait_event("closed")
            self.assertTrue(call.closed.is_set())
        finally:
            peer.close()
            fake.close()


if __name__ == "__main__":
    unittest.main()
