#!/usr/bin/env bash
# Download verified fixtures, then test an image without provider/network access.
set -euo pipefail
umask 077
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	echo "usage: $0 IMAGE_REFERENCE [linux/amd64|linux/arm64]" >&2
	exit 2
fi
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d /tmp/voxmail-model-acceptance.XXXXXX)
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$root"
go run ./tests/models "$work" >"$work/models.json"
cat "$work/models.json"
piper=$(jq -er '.piper_model' "$work/models.json")
whisper=$(jq -er '.whisper_model' "$work/models.json")
platform_args=()
if [ "$#" = 2 ]; then
	case "$2" in linux/amd64|linux/arm64) ;; *) echo "unsupported native platform: $2" >&2; exit 2 ;; esac
	platform_args+=(--platform "$2")
fi
# Pull before disabling the container network. Local CI images already exist.
docker image inspect "$1" >/dev/null 2>&1 || docker pull "${platform_args[@]}" "$1"
docker run --rm --network none \
	"${platform_args[@]}" \
	--user "$(id -u):$(id -g)" \
	--mount "type=bind,src=$work,dst=/models,readonly" \
	-e "VOXMAIL_PIPER_MODEL=/models/$piper" \
	-e "VOXMAIL_WHISPER_MODEL=/models/$whisper" \
	--entrypoint /bin/sh "$1" -ec \
	'/usr/local/bin/voxmail-runtime-smoke; /usr/local/bin/voxmail-real-model-smoke'
