#!/bin/sh
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 BACKUP.tar.gz [IMAGE@sha256:DIGEST]" >&2
  exit 2
fi
archive=$1
image=${2:-${VOXMAIL_IMAGE:-}}
key=${VOXMAIL_ENCRYPTION_KEY:-}
tool_image=${VOXMAIL_BACKUP_TOOL_IMAGE:-busybox:1.36.1}

if [ ! -f "$archive" ]; then
  echo "backup archive not found: $archive" >&2
  exit 1
fi
case "$image" in
  *@sha256:*) ;;
  *) echo "restore drill requires an immutable image digest (IMAGE@sha256:...)" >&2; exit 2 ;;
esac
if [ "${#key}" -lt 32 ]; then
  echo "VOXMAIL_ENCRYPTION_KEY must contain at least 32 bytes" >&2
  exit 2
fi

archive_dir=$(CDPATH= cd -- "$(dirname -- "$archive")" && pwd)
archive_name=$(basename -- "$archive")
run_id=${VOXMAIL_RESTORE_DRILL_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}
case "$run_id" in
  ''|*[!A-Za-z0-9_.-]*) echo "invalid VOXMAIL_RESTORE_DRILL_ID" >&2; exit 2 ;;
esac
volume="voxmail-restore-drill-$run_id"
container="voxmail-restore-drill-$run_id"

if docker volume inspect "$volume" >/dev/null 2>&1; then
  echo "refusing to reuse existing restore volume: $volume" >&2
  exit 1
fi
docker volume create --label voxmail.restore-drill=1 "$volume" >/dev/null

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  if [ "${VOXMAIL_KEEP_RESTORE_DRILL:-0}" != 1 ]; then
    docker volume rm "$volume" >/dev/null 2>&1 || true
  else
    echo "kept restore volume $volume"
  fi
}
trap cleanup EXIT INT TERM

docker run --rm \
  --mount "type=volume,source=$volume,target=/data" \
  --mount "type=bind,source=$archive_dir,target=/archive,readonly" \
  "$tool_image" sh -eu -c '
    tar xzf "/archive/$1" -C /data
    test -f /data/sqlite/voxmail.db
    test -d /data/mail
    test -d /data/voices
    test -d /data/prompts
    test -d /data/recordings
  ' sh "$archive_name"

docker run -d --name "$container" \
  --mount "type=volume,source=$volume,target=/data" \
  --env "VOXMAIL_ENCRYPTION_KEY=$key" \
  --env VOXMAIL_ENABLE_CALLS=0 \
  --env VOXMAIL_PROVISION_MODELS=0 \
  --publish 127.0.0.1::8080 \
  "$image" >/dev/null

port=""
for _ in $(seq 1 60); do
  port=$(docker port "$container" 8080/tcp 2>/dev/null | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -n 1 || true)
  if [ -n "$port" ] && curl -fsS --max-time 5 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
    echo "restore drill passed with $image using volume $volume"
    exit 0
  fi
  sleep 1
done

echo "restore drill did not reach healthz" >&2
docker logs "$container" >&2 || true
exit 1
