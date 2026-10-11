#!/usr/bin/env python3
"""Verify offline HTTP/worker admission with restored, enabled provider settings.

Uses a disposable volume and Docker's none network. Model provisioning and a
forced SIP account are deliberately enabled to verify offline precedence. No
production data, providers, model downloads or published ports are needed.
"""

import argparse
import hashlib
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import time
import uuid


def docker(*args, check=True):
    result = subprocess.run(["docker", *args], text=True, capture_output=True, timeout=90)
    if check and result.returncode:
        raise RuntimeError(result.stdout + result.stderr)
    return (result.stdout + result.stderr).strip()


def workload(phase):
    sys.path.insert(0, "/fixture")
    from tests.voices.smoke import Console
    admin = Console("http://127.0.0.1:8080")
    deadline = time.monotonic() + 30
    while True:
        try:
            ready = admin.request("GET", "/readyz")
            assert ready["offline_mode"] is True
            break
        except Exception:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.1)
    if phase == "setup":
        user = admin.request("POST", "/api/v1/setup", {"username": "offline-admin", "password": "OfflineFixturePassword-123", "pin": "123456"}, 201)
        account = admin.request("POST", "/api/v1/accounts", {
            "canonical_name": "Restored enabled mailbox", "email": "restored@example.test",
            "imap_host": "8.8.8.8", "imap_port": 993, "imap_user": "fixture", "imap_password": "fixture-imap",
            "imap_security": "implicit_tls", "smtp_host": "8.8.8.8", "smtp_port": 465,
            "smtp_user": "fixture", "smtp_password": "fixture-smtp", "smtp_security": "implicit_tls",
            "sync_interval_minutes": 1, "call_alert_enabled": True, "folder_map": {"INBOX": "Inbox"},
        }, 201)
        admin.request("PUT", "/api/v1/sip", {"domain": "sip.example.test", "username": "restored", "password": "fixture-sip", "enabled": True})
        # Seed persisted enabled alerts as a restored volume would contain.
        with sqlite3.connect("/data/sqlite/voxmail.db") as db:
            db.execute("UPDATE settings SET alerts_enabled=1, alert_phone='+15555550123' WHERE user_id=?", (user["id"],))
        Path("/data/offline-fixture.json").write_text(json.dumps({"account": account["id"]}))
    else:
        admin.request("POST", "/api/v1/login", {"username": "offline-admin", "password": "OfflineFixturePassword-123"})
    account = json.loads(Path("/data/offline-fixture.json").read_text())["account"]
    assert admin.request("GET", "/api/v1")["offline_mode"] is True
    assert admin.request("GET", "/api/v1/sip")["enabled"] is True
    assert admin.request("GET", "/api/v1/settings")["alerts_enabled"] is True
    for path, body in [
        ("/api/v1/accounts/validate", {"imap_host": "8.8.8.8"}),
        ("/api/v1/accounts/test", {"account_id": account}),
        ("/api/v1/accounts/" + account + "/sync", {}),
        ("/api/v1/alerts/test", {}),
        ("/api/v1/voices/install", {"voice": "en_US-lessac-low"}),
        ("/api/v1/recovery/request", {"email": "restored@example.test", "purpose": "reset_password"}),
        ("/api/v1/recovery/request", {"email": "unknown@example.test", "purpose": "bypass_2fa"}),
    ]:
        response = admin.request("POST", path, body, 503)
        assert "offline mode" in response["error"], (path, response)
    admin.request("POST", "/api/v1/contacts", {"name": "Local maintenance", "email": phase + "@example.test"}, 201)
    # Check durable work rather than relying on an unreachable test network to
    # make accidentally started provider workers look like offline success.
    time.sleep(1)
    with sqlite3.connect("/data/sqlite/voxmail.db") as db:
        for table in ("sync_runs", "voice_install_jobs", "recovery_tokens"):
            assert db.execute("SELECT COUNT(*) FROM " + table).fetchone()[0] == 0, table
        assert db.execute("SELECT enabled FROM sip_settings").fetchone()[0] == 1
        assert db.execute("SELECT alerts_enabled FROM settings").fetchone()[0] == 1
        assert db.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
    processes = [Path(p).read_text(errors="replace").replace("\x00", " ") for p in Path("/proc").glob("[0-9]*/cmdline")]
    assert not any(line.split(" ")[0].endswith(("/baresip", "/mbsync", "/whisper-cli")) for line in processes), processes
    assert not Path("/data/run/baresip.sock").exists(), "offline created call bridge"
    print(json.dumps({"phase": phase, "offline_mode": True, "enabled_settings_retained": True, "provider_jobs": 0, "manual_provider_requests": "503", "local_edits": "ok"}), flush=True)


def host(image, binary):
    repo = Path(__file__).resolve().parents[2]
    image_id = docker("image", "inspect", "--format", "{{.Id}}", image)
    name = "voxmail-offline-" + uuid.uuid4().hex[:12]
    volume = name + "-data"
    docker("volume", "create", volume)
    try:
        command = ["run", "--detach", "--name", name, "--network", "none",
                   "--mount", f"type=volume,source={volume},target=/data",
                   "--mount", f"type=bind,source={repo},target=/fixture,readonly",
                   "--env", "VOXMAIL_ENCRYPTION_KEY=offline-disposable-key-32-bytes-long",
                   "--env", "VOXMAIL_OFFLINE_MODE=true", "--env", "VOXMAIL_PROVISION_MODELS=true",
                   "--env", 'VOXMAIL_SIP_ACCOUNT=<sip:forced@8.8.8.8>;auth_user=forced;auth_pass=fixture;regint=10']
        identity = {}
        if binary:
            binary = binary.resolve()
            if not binary.stat().st_mode & 0o001:
                raise RuntimeError("mounted binary must be executable by the container user")
            identity["application_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
            command.extend(["--mount", f"type=bind,source={binary},target=/usr/local/bin/voxmail,readonly"])
        docker(*command, image_id)
        print(docker("exec", name, "python3", "/fixture/tests/offline/smoke.py", "--workload", "setup"), flush=True)
        docker("restart", "--time", "5", name)
        print(docker("exec", name, "python3", "/fixture/tests/offline/smoke.py", "--workload", "restored"), flush=True)
        assert json.loads(docker("inspect", "--format", "{{json .NetworkSettings.Networks}}", name)).keys() == {"none"}
        logs = docker("logs", name)
        assert "mail sync failed" not in logs and "baresip did not start" not in logs, logs
        docker("stop", "--time", "5", name)
        assert docker("inspect", "--format", "{{.State.ExitCode}}", name) == "0"
        print(json.dumps({"image": image_id, **identity, "network": "none", "forced_sip_and_provisioning_suppressed": True, "restart_and_shutdown": "ok"}), flush=True)
    except BaseException:
        print(docker("logs", name, check=False), file=sys.stderr)
        raise
    finally:
        docker("rm", "--force", name, check=False)
        docker("volume", "rm", volume, check=False)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image", nargs="?")
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--workload", choices=["setup", "restored"])
    args = parser.parse_args()
    if args.workload:
        workload(args.workload)
    elif args.image:
        host(args.image, args.binary)
    else:
        parser.error("provide a local image to test")
