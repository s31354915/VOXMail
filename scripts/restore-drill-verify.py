#!/usr/bin/env python3
"""Read-only checks for a restored VOXMail volume running offline."""

from __future__ import annotations

from email import policy
from email.parser import BytesParser
import json
import os
from pathlib import Path
import sqlite3
import subprocess


def fail(message: str) -> None:
    raise SystemExit(message)


if os.environ.get("VOXMAIL_OFFLINE_MODE", "").strip().lower() not in {"1", "true", "yes", "on"}:
    fail("restore verifier requires VOXMAIL_OFFLINE_MODE")

database = Path("/data/sqlite/voxmail.db")
mail_root = Path("/data/mail").resolve()
if not database.is_file():
    fail("restored database is missing")

with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as db:
    integrity = db.execute("PRAGMA integrity_check").fetchone()[0]
    if integrity != "ok":
        fail(f"database integrity check failed: {integrity}")
    foreign_keys = db.execute("PRAGMA foreign_key_check").fetchall()
    if foreign_keys:
        fail(f"database contains {len(foreign_keys)} foreign-key violations")
    users = db.execute("SELECT COUNT(*) FROM users").fetchone()[0]
    accounts = db.execute("SELECT id, imap_password, smtp_password FROM accounts WHERE COALESCE(deleting,0)=0").fetchall()
    messages = db.execute("SELECT id, path FROM mail_messages ORDER BY id").fetchall()
    sip_row = db.execute("SELECT enabled FROM sip_settings WHERE id=1").fetchone()
    sip_enabled = bool(sip_row and sip_row[0])
    alert_profiles = db.execute("SELECT COUNT(*) FROM settings WHERE alerts_enabled<>0").fetchone()[0]

    if os.environ.get("VOXMAIL_RESTORE_DRILL_EXPECT_ENABLED_NETWORK_SETTINGS") == "1":
        if not sip_enabled or alert_profiles < 1:
            fail("test fixture must have both SIP and call alerts enabled in the restored database")

    if users < 1:
        fail("restored database has no users")
    if not accounts:
        fail("restored database has no mail accounts")
    if not messages:
        fail("restored database has no indexed mail")

    for account_id, imap_password, smtp_password in accounts:
        if not imap_password or not smtp_password:
            fail(f"account {account_id} is missing an encrypted mail credential")
        checked = subprocess.run(
            ["/usr/local/bin/voxmail-secret", "--check-account", account_id],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        if checked.returncode:
            fail(f"account {account_id} credentials could not be decrypted: {checked.stderr.strip()}")

    if sip_enabled:
        checked = subprocess.run(
            ["/usr/local/bin/voxmail-secret", "--check-sip"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        if checked.returncode:
            fail(f"enabled SIP credentials could not be decrypted: {checked.stderr.strip()}")

    readable_mail = 0
    readable_attachments = 0
    for message_id, raw_path in messages:
        path = Path(raw_path)
        if not path.is_absolute():
            path = Path("/data") / path
        try:
            resolved = path.resolve(strict=True)
            resolved.relative_to(mail_root)
            raw = resolved.read_bytes()
            if not raw:
                continue
            parsed = BytesParser(policy=policy.default).parsebytes(raw)
        except (OSError, ValueError):
            continue
        readable_mail += 1
        expected = db.execute(
            "SELECT filename, size FROM attachment_metadata WHERE message_id=? ORDER BY id",
            (message_id,),
        ).fetchall()
        if not expected:
            continue
        actual = []
        for part in parsed.walk():
            if part.is_multipart():
                continue
            filename = part.get_filename()
            if not filename and part.get_content_disposition() != "attachment":
                continue
            payload = part.get_payload(decode=True) or b""
            actual.append((filename or "", len(payload)))
        if all(any(name == actual_name and size == actual_size for actual_name, actual_size in actual) for name, size in expected):
            readable_attachments += len(expected)
            break

if readable_mail == 0:
    fail("no indexed mail file was readable beneath /data/mail")
if readable_attachments == 0:
    fail("no indexed attachment payload was readable from restored mail")

for cmdline_path in Path("/proc").glob("[0-9]*/cmdline"):
    try:
        cmdline = cmdline_path.read_bytes()
    except OSError:
        continue
    if b"/usr/local/bin/baresip" in cmdline or b"/fixture/tests/shutdown/baresip.sh" in cmdline:
        fail("offline restore started a Baresip process")

print(json.dumps({
    "database_integrity": integrity,
    "foreign_key_violations": 0,
    "users": users,
    "accounts": len(accounts),
    "mail_files_readable": readable_mail,
    "attachment_payloads_readable": readable_attachments,
    "sip_settings_enabled_in_backup": sip_enabled,
    "alert_profiles_enabled_in_backup": alert_profiles,
    "offline_mode": True,
    "baresip_started": False,
}, sort_keys=True))
