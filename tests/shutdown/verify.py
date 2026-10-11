#!/usr/bin/env python3
"""Verify durable fixture state after the application container exits."""

import json
from pathlib import Path
import sqlite3

proof = Path("/proof")
active = json.loads((proof / "ready.json").read_text())
with sqlite3.connect("/data/sqlite/voxmail.db") as db:
    integrity = db.execute("PRAGMA integrity_check").fetchone()[0]
    sync = db.execute("SELECT success, finished_at FROM sync_runs WHERE id=?", (active["sync_id"],)).fetchone()
    manual_sync = db.execute("SELECT success, finished_at FROM sync_runs WHERE id=?", (active["manual_sync_id"],)).fetchone()
    job = db.execute("SELECT status, progress FROM voice_install_jobs WHERE id=?", (active["job_id"],)).fetchone()
    users = db.execute("SELECT COUNT(*) FROM users WHERE id=?", (active["user_id"],)).fetchone()[0]
if integrity != "ok" or users != 1:
    raise SystemExit("populated database was not preserved")
if Path(active["mail_path"]).read_bytes() != b"From: fixture@example.com\r\nSubject: Shutdown fixture\r\n\r\nRetained mail.\r\n":
    raise SystemExit("representative mail was not preserved")
if Path(active["recording_path"]).read_bytes() != b"retained recording fixture":
    raise SystemExit("representative recording was not preserved")
if not sync or sync[0] != 0 or not sync[1]:
    raise SystemExit(f"canceled sync was not finalized before exit: {sync}")
if not manual_sync or manual_sync[0] != 0 or not manual_sync[1]:
    raise SystemExit(f"canceled HTTP sync was not finalized before exit: {manual_sync}")
if job != ("failed", 100):
    raise SystemExit(f"canceled voice job was not finalized before exit: {job}")
if not (proof / "call-closed").exists():
    raise SystemExit("SIP peer did not observe the active call closing before container exit")
if any(path.name.startswith((".voice-install-", ".model-")) for path in Path("/data/voices").rglob("*")):
    raise SystemExit("voice download left a partial artifact")
if Path("/data/run/voxmail.pid").exists():
    raise SystemExit("obsolete PID file was recreated")
print(json.dumps({"database": integrity, "mail_preserved": True, "recording_preserved": True,
                  "sync_finalized": True, "manual_sync_finalized": True, "voice_finalized": True, "active_sip_call_closed": True}, sort_keys=True))
