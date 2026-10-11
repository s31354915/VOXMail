#!/usr/bin/env python3
"""Opt-in real Piper administrator-install acceptance on disposable Docker data.

Fetches a public Piper voice outside the bundled catalog. No mail or SIP
provider is configured. The named container and volume are removed on exit.
"""

import argparse
import http.cookiejar
import json
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request
import uuid


class Console:
    def __init__(self, base):
        self.base = base
        self.csrf = ""
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(self, method, path, body=None, expected=200, audio=False):
        headers = {"Content-Type": "application/json"}
        if self.csrf:
            headers["X-CSRF-Token"] = self.csrf
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.base + path, data=data, method=method, headers=headers)
        try:
            response = self.opener.open(request, timeout=120)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            payload = response.read()
            self.csrf = response.headers.get("X-CSRF-Token", self.csrf)
            if response.status != expected:
                raise RuntimeError(f"{method} {path}: HTTP {response.status}, expected {expected}: {payload.decode(errors='replace')}")
            return payload if audio else json.loads(payload)


def docker(*args, check=True):
    result = subprocess.run(["docker", *args], check=check, text=True, capture_output=True)
    return (result.stdout + result.stderr).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--binary", type=Path, help="optional current application binary, mounted read-only")
    parser.add_argument("--port", type=int, default=18088)
    parser.add_argument("--voice", default="en_US-lessac-low")
    args = parser.parse_args()
    if args.binary and not args.binary.stat().st_mode & 0o001:
        raise SystemExit("mounted binary must be executable by the container user (chmod 755)")
    if not 1024 <= args.port <= 65535:
        raise SystemExit("port must be between 1024 and 65535")
    token = uuid.uuid4().hex[:12]
    name = f"voxmail-voice-{token}"
    volume = f"voxmail-voice-data-{token}"
    docker("volume", "create", volume)
    try:
        command = ["run", "--detach", "--name", name, "--network", "bridge",
                   "--publish", f"127.0.0.1:{args.port}:8080",
                   "--mount", f"type=volume,source={volume},target=/data",
                   "--env", "VOXMAIL_ENCRYPTION_KEY=disposable-voice-test-key-32-bytes-long"]
        if args.binary:
            command.extend(["--mount", f"type=bind,source={args.binary.resolve()},target=/usr/local/bin/voxmail,readonly"])
        command.append(args.image)
        docker(*command)
        base = f"http://127.0.0.1:{args.port}"
        deadline = time.monotonic() + 45
        while True:
            try:
                with urllib.request.urlopen(base + "/readyz", timeout=1) as response:
                    if response.status == 200:
                        break
            except (urllib.error.URLError, OSError):
                if docker("inspect", "--format", "{{.State.Running}}", name) != "true":
                    raise RuntimeError("disposable application exited before readiness")
                if time.monotonic() >= deadline:
                    raise RuntimeError("disposable application did not become ready")
                time.sleep(0.2)
        admin = Console(base)
        admin.request("POST", "/api/v1/setup", {"username": "voice-admin", "password": "VoiceAdminPassword-123", "pin": "2468"}, 201)
        job = admin.request("POST", "/api/v1/voices/install", {"voice": args.voice}, 202)
        print(f"Administrator accepted installation of {args.voice}", flush=True)
        deadline = time.monotonic() + 600
        previous = None
        while True:
            status = admin.request("GET", "/api/v1/voices/jobs/" + job["job_id"])
            state = status["stage"], status["progress"]
            if state != previous:
                print(f"Installation: {state[0]} {state[1]}%", flush=True)
                previous = state
            if status["status"] == "failed":
                raise RuntimeError(status["error"])
            if status["status"] == "complete":
                break
            if time.monotonic() >= deadline:
                raise RuntimeError("administrator installation did not complete")
            time.sleep(1)
        audio = admin.request("POST", "/api/v1/voices/preview", {"voice": args.voice, "speed": 3}, audio=True)
        if not audio.startswith(b"RIFF") or audio[8:12] != b"WAVE" or len(audio) <= 44:
            raise RuntimeError("installed voice did not produce real WAV audio")
        admin.request("POST", "/api/v1/users", {"username": "voice-user", "password": "VoiceUserPassword-123", "pin": "1357", "role": "user"}, 201)
        user = Console(base)
        user.request("POST", "/api/v1/login", {"username": "voice-user", "password": "VoiceUserPassword-123"})
        for voice in [args.voice, "https://models.example/custom.onnx"]:
            user.request("POST", "/api/v1/voices/install", {"voice": voice}, 403)
        voices = user.request("GET", "/api/v1/voices")
        if not any(voice["voice"] == args.voice and voice["installed"] for voice in voices):
            raise RuntimeError("regular user cannot see the administrator-installed voice")
        user.request("PUT", "/api/v1/settings", {"tts_voice": args.voice, "menu_speed": 3, "email_speed": 2})
        user_audio = user.request("POST", "/api/v1/voices/preview", {"voice": args.voice, "speed": 3}, audio=True)
        if not user_audio.startswith(b"RIFF") or len(user_audio) <= 44:
            raise RuntimeError("regular user cannot preview the installed voice")
        # Inspect the actual activated pair independently of the API status.
        inspect = """import hashlib,json,pathlib,sqlite3,sys
root=pathlib.Path('/data/voices')
manifest=json.loads((root/'.active'/(sys.argv[1]+'.json')).read_text())
for field in ('model','config'):
    data=(root/manifest[field]).read_bytes()
    assert hashlib.sha256(data).hexdigest()==manifest[field+'_sha256']
db=sqlite3.connect('/data/sqlite/voxmail.db')
assert db.execute('SELECT COUNT(*) FROM voice_install_jobs').fetchone()[0]==1
print(json.dumps({'voice':manifest['voice'],'model_sha256':manifest['model_sha256'],'config_sha256':manifest['config_sha256']}))
"""
        print(docker("exec", name, "python3", "-c", inspect, args.voice), flush=True)
        docker("stop", "--time", "5", name)
        print(f"Real Piper voice acceptance passed: administrator install, digest-verified pair, {len(audio)}-byte WAV preview, user install denial and installed-voice selection/preview.", flush=True)
    except BaseException:
        logs = docker("logs", name, check=False)
        if logs:
            print(logs, flush=True)
        raise
    finally:
        docker("rm", "--force", name, check=False)
        docker("volume", "rm", volume, check=False)


if __name__ == "__main__":
    main()
