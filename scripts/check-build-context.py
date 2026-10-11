#!/usr/bin/env python3
"""Verify real Docker context filtering and Go builder contents with fake secrets.

Only application source is copied into a disposable fixture. Operator data and
review documents are represented by harmless markers; real private files are
never used to test whether an exclusion works. No image is loaded or published.
"""

import argparse
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


PRIVATE = [
    ".git/context-marker", "data/context-marker", "bin/context-marker",
    "build/context-marker", "backups/context-marker", "nested/backups/context-marker",
    "custom-backup.tar.gz", "custom-backup.tgz", "custom-backup.zip",
    "private.db", "private.db-shm", "private.db-wal", "private.sqlite", "private.sqlite3",
    ".env", ".env.production", "nested/.env.local", "diagnostics.log",
    "tests/browser/node_modules/context-marker", "tests/browser/test-results/context-marker",
    "tests/browser/playwright-report/context-marker", "tests/sippeer/__pycache__/context-marker",
    "tests/sippeer/cache.pyc", "nested/.pytest_cache/context-marker",
    "nested/.venv/context-marker", "nested/coverage/context-marker",
    ".codex/context-marker", ".vscode/context-marker", "b.pid",
    "internal/calls/drafts/context-marker", "plan-astra.md", "nested/plan-astra.md",
    "pasted-text-1.txt", "docs/REVIEW-SOURCE.md", "docs/REVIEW-PROGRESS.md",
]
CONSOLE_SOURCES = [
    "internal/web/static/console.bundle.js", "internal/web/console/console.css",
]
REQUIRED_CONTEXT = CONSOLE_SOURCES + [
    "go.mod", "go.sum", "cmd/voxmail/main.go", "internal/web/static/index.html",
    "baresip/shim/voxmail.c", "assets/welcome.wav", "assets/main-menu.wav",
    "assets/static-prompts.json", "scripts/entrypoint.sh", "scripts/runtime-smoke.sh",
    "scripts/real-model-smoke.sh", "scripts/sip-peer-smoke.sh", "tests/sippeer/smoke.py",
    ".env.example", "context-only.marker", "requirements/python.lock",
]


def copy_source(root, context):
    # Explicit source extensions prevent this acceptance fixture from reading
    # actual runtime data, credentials, browser installations or review notes.
    for directory, extensions in {
        "cmd": {".go"}, "internal": {".go", ".html", ".json", ".wav"},
        "baresip/shim": {".c", ".h", ".txt"}, "assets": {".wav", ".json"},
        "scripts": {".sh", ".py"}, "tests/sippeer": {".py"},
        "requirements": {".in", ".lock"},
    }.items():
        for path in (root / directory).rglob("*"):
            if path.is_file() and not path.is_symlink() and path.suffix in extensions:
                if {"drafts", "node_modules", "__pycache__", "test-results", "playwright-report"}.intersection(path.relative_to(root).parts):
                    continue
                target = context / path.relative_to(root)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(path, target)
    # Copy only the served bundle, stylesheet and embedded images, not the
    # console's build tools or installed npm packages.
    for name in CONSOLE_SOURCES:
        target = context / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(root / name, target)
    for path in (root / "internal/web/static/assets").iterdir():
        if path.is_file() and not path.is_symlink() and path.suffix in {".svg", ".png", ".webp"}:
            target = context / path.relative_to(root)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, target)
    for name in ["go.mod", "go.sum", ".env.example"]:
        shutil.copyfile(root / name, context / name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--baseline", help="negative control: use this git revision's Dockerfile and ignore rules")
    args = parser.parse_args()
    root = args.root.resolve()
    if args.baseline:
        def read(name):
            return subprocess.check_output(["git", "show", f"{args.baseline}:{name}"], cwd=root, text=True)
    else:
        def read(name):
            return (root / name).read_text()
    dockerfile = read("Dockerfile")
    stage = re.search(r"^FROM golang:[^\n]+ AS build\n(.*?)(?=^FROM |\Z)", dockerfile, re.M | re.S)
    if stage is None:
        raise SystemExit("production Go builder stage not found")
    with tempfile.TemporaryDirectory(prefix="voxmail-context-check-", dir="/tmp") as work:
        work = Path(work)
        context = work / "context"
        context.mkdir()
        copy_source(root, context)
        (context / ".dockerignore").write_text(read(".dockerignore"))
        for name in PRIVATE + ["context-only.marker"]:
            path = context / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("VOXMAIL HARMLESS CONTEXT MARKER\n")
        inspect = work / "Dockerfile.inspect"
        inspect.write_text("# syntax=docker/dockerfile:1\n" + stage.group(0) +
                           "\nFROM scratch AS inspect\nCOPY --from=build /src /builder\nCOPY . /context\n")
        output = work / "output"
        subprocess.run(["docker", "buildx", "build", "--progress=plain", "--target", "inspect",
                        "--build-arg", "TARGETARCH=amd64", "--output", f"type=local,dest={output}",
                        "--file", str(inspect), str(context)], cwd=root, check=True)
        failures = []
        for name in PRIVATE:
            for location in ["context", "builder"]:
                if (output / location / name).exists():
                    failures.append(f"private marker reached {location}: {name}")
        for name in REQUIRED_CONTEXT:
            if not (output / "context" / name).is_file():
                failures.append(f"required source missing from effective context: {name}")
        for name in CONSOLE_SOURCES + ["go.mod", "go.sum", "cmd/voxmail/main.go", "internal/web/static/index.html"]:
            if not (output / "builder" / name).is_file():
                failures.append(f"application source missing from Go builder: {name}")
        for name in ["context-only.marker", "assets", "scripts", "tests", ".env.example"]:
            if (output / "builder" / name).exists():
                failures.append(f"non-application material reached Go builder: {name}")
        if failures:
            raise SystemExit("\n".join(failures))
        print(f"Build context verified: {len(PRIVATE)} private markers excluded; "
              "runtime sources retained; production Go builder compiled all three binaries and contains only application source.")


if __name__ == "__main__":
    main()
