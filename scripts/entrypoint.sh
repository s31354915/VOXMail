#!/bin/sh
set -eu

mkdir -p /data/config /data/sqlite /data/run /data/mail /data/recordings /data/voices /data/whisper /data/logs /data/run/voxmail
chmod 700 /data /data/config /data/sqlite /data/run /data/mail /data/recordings /data/voices /data/whisper /data/logs /data/run/voxmail

if [ -z "${VOXMAIL_ENCRYPTION_KEY:-}" ]; then
  echo 'VOXMAIL_ENCRYPTION_KEY must be provided through Docker secrets or the environment' >&2
  exit 1
fi

baresip_pid=""
voxmail_pid=""
if [ -n "${VOXMAIL_SIP_ACCOUNT:-}" ]; then
  baresip_dir=/data/config/baresip
  mkdir -p "$baresip_dir"
  if [ ! -f "$baresip_dir/config" ]; then
    cat > "$baresip_dir/config" <<'EOF'
module_path /usr/local/lib/baresip/modules
module_app voxmail.so
module account.so
module g711.so
module auconv.so
module auresamp.so
module aubridge.so
module aufile.so
module in_band_dtmf.so
module ice.so
module stun.so
module srtp.so
module dtls_srtp.so
module stdio.so
audio_source voxmail
audio_player voxmail
audio_buffer 60-200
rtp_ports 10000-10100
call_max_calls 10
EOF
  fi
  printf '%s\n' "$VOXMAIL_SIP_ACCOUNT" > "$baresip_dir/accounts"
  /usr/local/bin/baresip -f "$baresip_dir" > /data/logs/baresip.log 2>&1 &
  baresip_pid=$!
fi

term() {
  if [ -n "$baresip_pid" ]; then kill "$baresip_pid" 2>/dev/null || true; fi
  if [ -n "$voxmail_pid" ]; then kill "$voxmail_pid" 2>/dev/null || true; fi
}
trap term INT TERM EXIT

/usr/local/bin/voxmail &
voxmail_pid=$!
wait "$voxmail_pid"
