#!/bin/sh
# Opt-in native Baresip/SIP/RTP acceptance smoke test.
#
# This launches a real Baresip process and the real VOXMail module against a
# disposable localhost SIP peer. It is intentionally separate from the fast
# Go/native unit suites because it needs the final image's native runtime.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
python_bin=${VOXMAIL_SIP_PEER_PYTHON:-python3}
baresip_bin=${VOXMAIL_SIP_PEER_BARESIP:-baresip}
python_script=${VOXMAIL_SIP_PEER_SCRIPT:-}

if [ -z "$python_script" ]; then
  if [ -f "$script_dir/../tests/sippeer/smoke.py" ]; then
    python_script="$script_dir/../tests/sippeer/smoke.py"
  else
    python_script=/usr/local/lib/voxmail/sippeer/smoke.py
  fi
fi

command -v "$python_bin" >/dev/null 2>&1 || {
  echo "sip-peer-smoke: missing Python interpreter: $python_bin" >&2
  exit 2
}

test -f "$python_script" || {
  echo "sip-peer-smoke: missing peer driver: $python_script" >&2
  exit 2
}

exec "$python_bin" "$python_script" --baresip "$baresip_bin"
