#!/usr/bin/env bash
# Exercise current Go diagnostic paths with the supplied image's real engines.
set -euo pipefail
umask 077
if [ "$#" != 1 ]; then
  echo "usage: $0 LOCAL_IMAGE_REFERENCE" >&2
  exit 2
fi
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d /tmp/voxmail-verbose-test.XXXXXX)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$root"
mkdir -p "$work/models"
go run ./tests/models "$work/models" > "$work/models.json"
CGO_ENABLED=0 go build -o "$work/acceptance" ./tests/verbose
piper=$(jq -er '.piper_model' "$work/models.json")
whisper=$(jq -er '.whisper_model' "$work/models.json")
for mode in false true; do
  docker run --rm --network none --user "$(id -u):$(id -g)" \
    --mount "type=bind,src=$work/models,dst=/models,readonly" \
    --mount "type=bind,src=$work/acceptance,dst=/acceptance,readonly" \
    -e "VOXMAIL_VERBOSE=$mode" \
    -e "VOXMAIL_PIPER_MODEL=/models/$piper" \
    -e "VOXMAIL_WHISPER_MODEL=/models/$whisper" \
    --entrypoint /acceptance "$1"
  docker run --rm --network none \
    --mount "type=bind,src=$root/tests/sippeer,dst=/fixture,readonly" \
    -e "VOXMAIL_VERBOSE=$mode" --entrypoint python3 "$1" \
    /fixture/smoke.py --shutdown-cycles 2
 done
