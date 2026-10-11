#!/usr/bin/env python3
"""Capture exact scan identity and report integrity without masking scan failure."""
import hashlib
import json
import os
import pathlib
import re
import sys


def main():
    if len(sys.argv) != 3 or sys.argv[1] not in ("prepare", "finish"):
        raise SystemExit("usage: scan-evidence.py prepare|finish DIRECTORY")
    action, directory = sys.argv[1], pathlib.Path(sys.argv[2])
    manifest = directory / "identity.json"
    if action == "prepare":
        image, source, platform = (os.environ[name] for name in ("SCAN_IMAGE", "SOURCE_SHA", "SCAN_PLATFORM"))
        if not re.fullmatch(r"[0-9a-f]{40}", source) or not re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", image):
            raise SystemExit("invalid immutable scan identity")
        if platform not in ("linux/amd64", "linux/arm64"):
            raise SystemExit("unsupported scan platform")
        directory.mkdir(parents=True, exist_ok=True)
        data = dict(source_sha=source, image=image, platform=platform,
                    run_id=os.environ.get("GITHUB_RUN_ID", ""),
                    run_attempt=os.environ.get("GITHUB_RUN_ATTEMPT", ""),
                    scanner="trivy", severity=["HIGH", "CRITICAL"], ignore_unfixed=True,
                    scan_outcome="not_run", report_status="pending")
    else:
        data = json.loads(manifest.read_text())
        data["scan_outcome"] = os.environ.get("SCAN_OUTCOME") or "not_run"
        report = directory / "trivy.json"
        try:
            raw = report.read_bytes()
            parsed = json.loads(raw)
            if not isinstance(parsed, dict) or not isinstance(parsed.get("SchemaVersion"), int) or not isinstance(parsed.get("Results", []), list):
                raise ValueError("invalid Trivy report structure")
            data.update(report_status="available", report_sha256=hashlib.sha256(raw).hexdigest(), report_bytes=len(raw))
        except FileNotFoundError:
            data["report_status"] = "missing"
        except (ValueError, UnicodeError):
            data["report_status"] = "invalid"
    manifest.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    if action == "finish" and data["report_status"] != "available":
        raise SystemExit("scan report " + data["report_status"] + "; publication must remain blocked")


if __name__ == "__main__":
    main()
