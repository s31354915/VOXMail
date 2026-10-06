#!/bin/sh
set -eu

service=${VOXMAIL_COMPOSE_SERVICE:-voxmail}
volume=${VOXMAIL_VOLUME_NAME:-voxmail-data}
backup_dir=${VOXMAIL_BACKUP_DIR:-backups}
tool_image=${VOXMAIL_BACKUP_TOOL_IMAGE:-busybox:1.36.1}

case "$volume" in
  ''|*[!A-Za-z0-9_.-]*) echo "invalid VOXMAIL_VOLUME_NAME" >&2; exit 2 ;;
esac
mkdir -p "$backup_dir"
backup_dir=$(CDPATH= cd -- "$backup_dir" && pwd)
archive_name="voxmail-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"

if ! docker volume inspect "$volume" >/dev/null 2>&1; then
  echo "volume does not exist: $volume" >&2
  exit 1
fi

was_running=0
if docker compose ps --status running --services 2>/dev/null | grep -Fxq "$service"; then
  was_running=1
  docker compose stop "$service"
fi

restart() {
  if [ "$was_running" -eq 1 ]; then
    docker compose start "$service" >/dev/null
  fi
}
trap restart EXIT INT TERM

docker run --rm \
  --mount "type=volume,source=$volume,target=/source,readonly" \
  --mount "type=bind,source=$backup_dir,target=/backup" \
  "$tool_image" sh -eu -c '
    test -f /source/sqlite/voxmail.db
    test -d /source/mail
    test -d /source/voices
    test -d /source/prompts
    tar czf "/backup/$1" -C /source .
  ' sh "$archive_name"

echo "created $backup_dir/$archive_name from volume $volume"
