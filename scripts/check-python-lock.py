#!/usr/bin/env python3
"""Verify both target wheel sets, offline native installation and hash rejection.

Run with Python 3.11 in the pinned runtime base. ARM wheel downloads verify hashes
and dependency closure; executing ARM binaries remains native-image acceptance.
All downloads, virtualenvs and negative fixtures are disposable under /tmp.
"""

import argparse
import pathlib
import re
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=pathlib.Path, default=pathlib.Path("requirements/python.lock"))
    args = parser.parse_args()
    if sys.version_info[:2] != (3, 11) or sys.platform != "linux":
        parser.error("use the pinned Linux Python 3.11 runtime base")
    lock = args.lock.resolve()
    with tempfile.TemporaryDirectory(prefix="voxmail-python-check-", dir="/tmp") as directory:
        directory = pathlib.Path(directory)
        for machine in ("x86_64", "aarch64"):
            platforms = []
            for glibc in range(36, 16, -1):
                platforms += ["--platform", f"manylinux_2_{glibc}_{machine}"]
            platforms += ["--platform", f"manylinux2014_{machine}"]
            subprocess.run([sys.executable, "-m", "pip", "download", "--no-cache-dir",
                            "--disable-pip-version-check", "--require-hashes", "--only-binary=:all:",
                            "--index-url", "https://pypi.org/simple", "--python-version", "3.11",
                            "--implementation", "cp", "--abi", "cp311", "--abi", "abi3", "--abi", "none",
                            *platforms, "--dest", str(directory / machine), "-r", str(lock)], check=True)
        # Native pip selects the correct wheel set; no index/network is involved
        # in either installation or the negative hash control.
        import platform
        native_wheels = directory / platform.machine()
        if not native_wheels.is_dir():
            parser.error("supported native targets are amd64 and arm64")
        environment = directory / "venv"
        subprocess.run([sys.executable, "-m", "venv", str(environment)], check=True)
        python = str(environment / "bin" / "python")
        subprocess.run([python, "-m", "pip", "install", "--no-index", "--find-links", str(native_wheels),
                        "--require-hashes", "--only-binary=:all:", "--upgrade", "-r", str(lock)], check=True)
        subprocess.run([python, "-m", "pip", "check"], check=True)
        subprocess.run([python, "-c", "import piper, numpy, onnxruntime; print(numpy.__version__, onnxruntime.__version__)"], check=True)
        subprocess.run([str(environment / "bin" / "piper"), "--help"], check=True, stdout=subprocess.DEVNULL)
        invalid = directory / "invalid.lock"
        invalid.write_text(re.sub(r"--hash=sha256:[0-9a-f]{64}", "--hash=sha256:" + "0" * 64, lock.read_text()))
        result = subprocess.run([sys.executable, "-m", "pip", "download", "--no-index", "--find-links", str(native_wheels),
                                 "--require-hashes", "--only-binary=:all:", "--dest", str(directory / "rejected"),
                                 "-r", str(invalid)], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if result.returncode == 0 or "DO NOT MATCH THE HASHES" not in result.stdout:
            raise SystemExit("tampered-hash negative control did not reject the package:\n" + result.stdout)
        print("Both target wheel hashes verified; native install/import/CLI and tampered-hash rejection passed.")


if __name__ == "__main__":
    main()
