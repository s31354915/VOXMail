#!/bin/sh
set -eu
umask 077

service=${VOXMAIL_COMPOSE_SERVICE:-voxmail}
volume=${VOXMAIL_VOLUME_NAME:-voxmail-data}
backup_dir=${VOXMAIL_BACKUP_DIR:-backups}
tool_image=${VOXMAIL_BACKUP_TOOL_IMAGE:-busybox:1.36.1}

case "$volume" in
  ''|*[!A-Za-z0-9_.-]*) echo "invalid VOXMAIL_VOLUME_NAME" >&2; exit 2 ;;
esac
mkdir -p "$backup_dir"
backup_dir=$(CDPATH= cd -- "$backup_dir" && pwd)
chmod 700 "$backup_dir"

if ! docker volume inspect "$volume" >/dev/null 2>&1; then
  echo "volume does not exist: $volume" >&2
  exit 1
fi

owner_uid=$(id -u)
owner_gid=$(id -g)
was_running=0
published=0
stage_dir=
archive_path=
checksum_path=
manifest_path=

if docker compose ps --status running --services 2>/dev/null | grep -Fxq "$service"; then
  was_running=1
fi

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [ -n "$stage_dir" ]; then
    if ! rm -rf "$stage_dir"; then
      status=1
    fi
  fi
  if [ "$published" -eq 0 ] && [ -n "$archive_path" ]; then
    if ! rm -f "$archive_path" "$checksum_path" "$manifest_path"; then
      status=1
    fi
  fi
  if [ "$was_running" -eq 1 ]; then
    if ! docker compose start "$service" >/dev/null; then
      echo "failed to restart service: $service" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
stage_dir=$(mktemp -d "$backup_dir/.voxmail-backup.XXXXXX")
chmod 700 "$stage_dir"
stage_id=${stage_dir##*.}
archive_name="voxmail-$timestamp-$stage_id.tar.gz"
archive_path="$backup_dir/$archive_name"
checksum_path="$archive_path.sha256"
manifest_path="$archive_path.manifest"
archive_stage="$stage_dir/$archive_name"
checksum_stage="$stage_dir/$archive_name.sha256"
manifest_stage="$stage_dir/$archive_name.manifest"

if [ "$was_running" -eq 1 ]; then
  docker compose stop "$service"
fi

docker run --rm \
  --mount "type=volume,source=$volume,target=/source,readonly" \
  --mount "type=bind,source=$stage_dir,target=/backup" \
  "$tool_image" sh -eu -c '
    umask 077
    test -f /source/sqlite/voxmail.db
    test -d /source/mail
    test -d /source/voices
    test -d /source/prompts
    tar czf "/backup/$1" -C /source .
    chmod 600 "/backup/$1"
    chown "$2:$3" "/backup/$1"
  ' sh "$archive_name" "$owner_uid" "$owner_gid"

test -s "$archive_stage"
checksum_line=$(sha256sum "$archive_stage")
archive_sha=${checksum_line%% *}
printf '%s  %s\n' "$archive_sha" "$archive_name" > "$checksum_stage"
printf 'format=voxmail-backup-v1\ncreated_at=%s\nvolume=%s\narchive=%s\nsha256=%s\n' \
  "$timestamp" "$volume" "$archive_name" "$archive_sha" > "$manifest_stage"
chmod 600 "$checksum_stage" "$manifest_stage"

# Publish metadata first and the complete archive last. The archive is the
# visible completion marker; a failed or interrupted run removes staged data.
mv "$manifest_stage" "$manifest_path"
mv "$checksum_stage" "$checksum_path"
mv "$archive_stage" "$archive_path"
published=1
echo "created $archive_path from volume $volume"
