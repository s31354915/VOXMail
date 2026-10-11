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
  *@sha256:*|sha256:*) ;;
  *) echo "restore drill requires an immutable image digest or local image ID" >&2; exit 2 ;;
esac
if [ "${#key}" -lt 32 ]; then
  echo "VOXMAIL_ENCRYPTION_KEY must contain at least 32 bytes" >&2
  exit 2
fi

archive_dir=$(CDPATH= cd -- "$(dirname -- "$archive")" && pwd)
archive_name=$(basename -- "$archive")
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

checksum_file="$archive.sha256"
manifest_file="$archive.manifest"
if [ -f "$checksum_file" ] || [ -f "$manifest_file" ]; then
  if [ ! -f "$checksum_file" ] || [ ! -f "$manifest_file" ]; then
    echo "backup checksum or manifest is missing: $archive" >&2
    exit 1
  fi
  checksum_name=$(basename -- "$checksum_file")
  manifest_format=$(sed -n 's/^format=//p' "$manifest_file")
  manifest_archive=$(sed -n 's/^archive=//p' "$manifest_file")
  manifest_sha=$(sed -n 's/^sha256=//p' "$manifest_file")
  checksum_sha=$(awk 'NR == 1 {print $1}' "$checksum_file")
  if [ "$manifest_format" != voxmail-backup-v1 ] || [ "$manifest_archive" != "$archive_name" ] || \
     [ -z "$checksum_sha" ] || [ "$manifest_sha" != "$checksum_sha" ]; then
    echo "backup manifest does not match the archive: $archive" >&2
    exit 1
  fi
  (CDPATH= cd -- "$archive_dir" && sha256sum -c "$checksum_name") || {
    echo "backup checksum verification failed: $archive" >&2
    exit 1
  }
else
  case "$archive_name" in
    voxmail-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.tar.gz)
      echo "warning: legacy backup has no checksum or manifest: $archive" >&2 ;;
    *) echo "backup checksum or manifest is missing: $archive" >&2; exit 1 ;;
  esac
fi

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

docker run --rm --network none \
  --mount "type=volume,source=$volume,target=/data" \
  --mount "type=bind,source=$archive_dir,target=/archive,readonly" \
  "$tool_image" sh -eu -c '
    tar xzf "/archive/$1" -C /data
    test -f /data/sqlite/voxmail.db
    test -d /data/mail
    test -d /data/voices
    test -d /data/prompts
    test -d /data/recordings
    chown -R 10001:10001 /data
  ' sh "$archive_name"

docker run -d --name "$container" \
  --network none \
  --mount "type=volume,source=$volume,target=/data" \
  --mount "type=bind,source=$script_dir,target=/restore-drill,readonly" \
  --env "VOXMAIL_ENCRYPTION_KEY=$key" \
  --env VOXMAIL_OFFLINE_MODE=1 \
  --env VOXMAIL_PROVISION_MODELS=0 \
  --env "VOXMAIL_RESTORE_DRILL_EXPECT_ENABLED_NETWORK_SETTINGS=${VOXMAIL_RESTORE_DRILL_EXPECT_ENABLED_NETWORK_SETTINGS:-0}" \
  "$image" >/dev/null

for _ in $(seq 1 60); do
  if docker exec "$container" curl -fsS --max-time 5 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    if docker exec "$container" curl -fsS --max-time 5 http://127.0.0.1:8080/readyz >/dev/null 2>&1 && \
       docker exec "$container" python3 /restore-drill/restore-drill-verify.py; then
      echo "restore drill passed offline with $image using volume $volume"
      exit 0
    fi
    echo "restore drill checks failed" >&2
    docker logs "$container" >&2 || true
    exit 1
  fi
  sleep 1
done

echo "restore drill did not reach healthz" >&2
docker logs "$container" >&2 || true
exit 1
