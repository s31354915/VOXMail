#!/bin/sh
set -eu
umask 077

mkdir -p /data/config /data/sqlite /data/run /data/mail /data/recordings /data/voices /data/whisper /data/logs /data/prompts /data/run/voxmail
chmod 700 /data /data/config /data/sqlite /data/run /data/mail /data/recordings /data/voices /data/whisper /data/logs /data/prompts /data/run/voxmail

if [ ! -s /data/prompts/welcome.wav ]; then
  cp /usr/local/share/voxmail/welcome.wav /data/prompts/welcome.wav
  chmod 600 /data/prompts/welcome.wav
fi
if [ ! -s /data/prompts/main-menu.wav ]; then
  cp /usr/local/share/voxmail/main-menu.wav /data/prompts/main-menu.wav
  chmod 600 /data/prompts/main-menu.wav
fi
if [ ! -s /data/prompts/static-prompts.json ]; then
  cp /usr/local/share/voxmail/static-prompts.json /data/prompts/static-prompts.json
  chmod 600 /data/prompts/static-prompts.json
fi

if [ -z "${VOXMAIL_ENCRYPTION_KEY:-}" ]; then
  echo 'VOXMAIL_ENCRYPTION_KEY must be provided through Docker secrets or the environment' >&2
  exit 1
fi

# baresip is spawned and supervised by the voxmail process itself, using the
# SIP account configured in the web console (or VOXMAIL_SIP_ACCOUNT override).

# Remove a stale pid file and, if it still points at a live process from an
# earlier run, stop it so orphaned children do not outlive the entrypoint.
if [ -f /data/run/voxmail.pid ]; then
  stale_pid="$(cat /data/run/voxmail.pid 2>/dev/null || true)"
  case "$stale_pid" in
    ''|*[!0-9]*) ;;
    *)
      if [ -r "/proc/$stale_pid/cmdline" ] && tr '\000' ' ' < "/proc/$stale_pid/cmdline" | grep -Fq '/usr/local/bin/voxmail'; then
        kill "$stale_pid" 2>/dev/null || true
      fi
      ;;
  esac
  rm -f /data/run/voxmail.pid
fi

voxmail_pid=""
cleanup() {
  if [ -n "$voxmail_pid" ]; then kill "$voxmail_pid" 2>/dev/null || true; fi
  rm -f /data/run/voxmail.pid
}
trap cleanup INT TERM EXIT

/usr/local/bin/voxmail &
voxmail_pid=$!
echo "$voxmail_pid" > /data/run/voxmail.pid
wait "$voxmail_pid"
