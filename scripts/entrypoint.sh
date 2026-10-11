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

# Older images wrote a PID into the persistent volume. It is not a process
# identity across container restarts, so discard it without signaling that PID.
rm -f /data/run/voxmail.pid

# Keep the application as the container's foreground process so Docker (or
# Compose's init process) delivers signals directly and waits for its shutdown.
exec /usr/local/bin/voxmail
