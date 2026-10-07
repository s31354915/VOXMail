#!/bin/sh
# Runtime acceptance checks for the final VOXMail image.
#
# This intentionally requires real model paths for the speech checks. A
# placeholder file proves only that the process can be started, not that the
# installed runtime can load a model and produce media.
set -eu

fail() {
  echo "runtime smoke failed: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"
}

check_elf_dependencies() {
  target=$1
  deps=$(ldd "$target" 2>&1 || true)
  case "$deps" in
    *"not found"*) fail "unresolved dependency in $target: $deps" ;;
  esac
}

for command_name in ldd sha256sum ffmpeg mbsync piper whisper-cli baresip voxmail-connect; do
  require_command "$command_name"
done

check_elf_dependencies /usr/local/bin/baresip
check_elf_dependencies /usr/local/bin/whisper-cli
check_elf_dependencies /usr/local/lib/libbaresip.so
for module in /usr/local/lib/baresip/modules/*.so; do
  test -f "$module" || fail "no baresip modules were installed"
  check_elf_dependencies "$module"
done

printf 'baresip: '
baresip -h 2>&1 | sed -n '1p'
printf 'whisper: '
whisper-cli -h 2>&1 | sed -n '1p'
printf 'ffmpeg: '
ffmpeg -version 2>&1 | sed -n '1p'
printf 'mbsync: '
mbsync --version 2>&1 | sed -n '1p'
piper --help >/dev/null 2>&1

smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/voxmail-runtime-smoke.XXXXXX")
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM

# Exercise the same raw-PCM conversion shape used by call playback and STT.
ffmpeg -v error -nostdin -y \
  -i /usr/local/share/voxmail/welcome.wav \
  -f s16le -ar 8000 -ac 1 "$smoke_dir/input.raw"
ffmpeg -v error -nostdin -y \
  -f s16le -ar 8000 -ac 1 -i "$smoke_dir/input.raw" \
  -ar 16000 -ac 1 -c:a pcm_s16le "$smoke_dir/converted.wav"
test -s "$smoke_dir/converted.wav" || fail "ffmpeg produced no converted audio"

piper_model=${VOXMAIL_PIPER_MODEL:-}
whisper_model=${VOXMAIL_WHISPER_MODEL:-}
whisper_fixture=${VOXMAIL_WHISPER_FIXTURE:-}
test -n "$piper_model" || fail 'VOXMAIL_PIPER_MODEL is required for the Piper smoke test'
test -n "$whisper_model" || fail 'VOXMAIL_WHISPER_MODEL is required for the Whisper smoke test'
test -s "$piper_model" || fail "Piper model is missing or empty: $piper_model"
test -s "${piper_model%.onnx}.onnx.json" || test -s "${piper_model}.json" || fail "Piper config is missing beside: $piper_model"
test -s "$whisper_model" || fail "Whisper model is missing or empty: $whisper_model"

printf 'piper_model_sha256=%s\n' "$(sha256sum "$piper_model" | awk '{print $1}')"
printf 'whisper_model_sha256=%s\n' "$(sha256sum "$whisper_model" | awk '{print $1}')"

# piper-tts 1.3.0 is line-oriented; this is the same one-shot worker contract
# used by internal/speech, including an actual WAV validation by ffmpeg.
printf '%s\n' 'VOXMail runtime smoke.' | \
  piper --model "$piper_model" --output_file "$smoke_dir/piper.wav"
test -s "$smoke_dir/piper.wav" || fail 'Piper produced no WAV'
ffmpeg -v error -nostdin -i "$smoke_dir/piper.wav" -f null -

if [ -n "$whisper_fixture" ]; then
  test -s "$whisper_fixture" || fail "Whisper fixture is missing or empty: $whisper_fixture"
else
  whisper_fixture=/usr/local/share/voxmail/welcome.wav
fi
whisper_output=$(whisper-cli -m "$whisper_model" -f "$whisper_fixture" --no-prints 2>"$smoke_dir/whisper.stderr" || {
  cat "$smoke_dir/whisper.stderr" >&2
  exit 1
})
if [ -n "${VOXMAIL_WHISPER_FIXTURE:-}" ] && [ -z "$whisper_output" ]; then
  fail 'Whisper returned empty output for the supplied real fixture'
fi
printf 'whisper_output=%s\n' "$(printf '%s' "$whisper_output" | tr '\n' ' ' | cut -c 1-240)"

# -t makes this a bounded module-load test. The config mirrors the production
# module list and the environment variables used by the Go supervisor.
mkdir -p "$smoke_dir/audio"
printf '%s\n' \
  'module_path /usr/local/lib/baresip/modules' \
  'module_app voxmail.so' \
  'module account.so' \
  'module g711.so' \
  'module auconv.so' \
  'module auresamp.so' \
  'module aubridge.so' \
  'module aufile.so' \
  'module in_band_dtmf.so' \
  'module ice.so' \
  'module stun.so' \
  'module srtp.so' \
  'module dtls_srtp.so' \
  'module stdio.so' \
  'call_accept yes' \
  'poll_method poll' \
  'audio_source voxmail,' \
  'audio_player voxmail,' \
  'audio_alert voxmail,' \
  'ausrc_srate 8000' \
  'auplay_srate 8000' \
  'ausrc_channels 1' \
  'auplay_channels 1' \
  'ausrc_format s16' \
  'auplay_format s16' \
  'audio_buffer 60-200' \
  'rtp_ports 10000-10100' \
  'call_max_calls 1' \
  'sip_listen 127.0.0.1:0' > "$smoke_dir/config"
VOXMAIL_CONTROL_SOCKET="$smoke_dir/baresip.sock" \
VOXMAIL_AUDIO_DIR="$smoke_dir/audio" \
  baresip -f "$smoke_dir" -t 1 >"$smoke_dir/baresip.log" 2>&1 || {
    cat "$smoke_dir/baresip.log" >&2
    fail 'baresip did not complete its bounded module-load run'
  }
grep -q 'baresip is ready' "$smoke_dir/baresip.log" || {
  cat "$smoke_dir/baresip.log" >&2
  fail 'baresip did not reach ready state'
}

if [ -n "${VOXMAIL_MBSYNC_CONFIG:-}" ]; then
  test -s "$VOXMAIL_MBSYNC_CONFIG" || fail 'VOXMAIL_MBSYNC_CONFIG is missing or empty'
  test -n "${VOXMAIL_MBSYNC_CHANNEL:-}" || fail 'VOXMAIL_MBSYNC_CHANNEL is required with VOXMAIL_MBSYNC_CONFIG'
  mbsync --list --config "$VOXMAIL_MBSYNC_CONFIG" "$VOXMAIL_MBSYNC_CHANNEL"
else
  echo 'mbsync config validation skipped; set VOXMAIL_MBSYNC_CONFIG and VOXMAIL_MBSYNC_CHANNEL for the configured check' >&2
fi

echo 'runtime smoke passed'
