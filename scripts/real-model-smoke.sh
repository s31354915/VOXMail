#!/bin/sh
# Opt-in final-image speech round-trip acceptance test.
#
# This intentionally requires real Piper and Whisper model files. It produces
# speech with the same one-shot Piper invocation used by internal/speech,
# converts it to the 16-bit/16 kHz/mono WAV shape required by whisper.cpp, and
# verifies that Whisper recognizes the supplied known phrase.
set -eu
umask 077

fail() {
  echo "real model smoke failed: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"
}

for command_name in ffmpeg ffprobe piper whisper-cli sha256sum; do
  require_command "$command_name"
done

piper_model=${VOXMAIL_PIPER_MODEL:-}
whisper_model=${VOXMAIL_WHISPER_MODEL:-}
spoken_text=${VOXMAIL_REAL_MODEL_TEXT:-This is a VOXMail speech smoke test.}
expected_text=${VOXMAIL_REAL_MODEL_EXPECTED:-speech smoke test}

test -n "$piper_model" || fail 'VOXMAIL_PIPER_MODEL is required'
test -n "$whisper_model" || fail 'VOXMAIL_WHISPER_MODEL is required'
test -n "$spoken_text" || fail 'VOXMAIL_REAL_MODEL_TEXT must not be empty'
test -n "$expected_text" || fail 'VOXMAIL_REAL_MODEL_EXPECTED must not be empty'
test -s "$piper_model" || fail "Piper model is missing or empty: $piper_model"
test -s "${piper_model%.onnx}.onnx.json" || test -s "${piper_model}.json" || fail "Piper config is missing beside: $piper_model"
test -s "$whisper_model" || fail "Whisper model is missing or empty: $whisper_model"

smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/voxmail-real-model.XXXXXX")
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM

printf 'piper_model_sha256=%s\n' "$(sha256sum "$piper_model" | awk '{print $1}')"
printf 'whisper_model_sha256=%s\n' "$(sha256sum "$whisper_model" | awk '{print $1}')"

# Keep this invocation aligned with speech.synthesizePiper: Piper 1.3.0 reads
# one plain-text line from stdin and reports completion by exiting successfully.
printf '%s\n' "$spoken_text" | piper --model "$piper_model" --output_file "$smoke_dir/piper.wav"
test -s "$smoke_dir/piper.wav" || fail 'Piper produced no WAV'
ffmpeg -v error -nostdin -i "$smoke_dir/piper.wav" -f null -

# whisper.cpp's CLI accepts 16-bit WAV input. Normalize Piper's model-specific
# sample rate before invoking the exact arguments used by internal/speech.
ffmpeg -v error -nostdin -y \
  -i "$smoke_dir/piper.wav" \
  -ar 16000 -ac 1 -c:a pcm_s16le "$smoke_dir/whisper.wav"

sample_rate=$(ffprobe -v error -select_streams a:0 -show_entries stream=sample_rate -of default=nw=1:nk=1 "$smoke_dir/whisper.wav")
channels=$(ffprobe -v error -select_streams a:0 -show_entries stream=channels -of default=nw=1:nk=1 "$smoke_dir/whisper.wav")
sample_fmt=$(ffprobe -v error -select_streams a:0 -show_entries stream=sample_fmt -of default=nw=1:nk=1 "$smoke_dir/whisper.wav")
test "$sample_rate" = 16000 || fail "converted WAV sample rate is $sample_rate, want 16000"
test "$channels" = 1 || fail "converted WAV channel count is $channels, want 1"
test "$sample_fmt" = s16 || fail "converted WAV sample format is $sample_fmt, want s16"

whisper_output=$(whisper-cli -m "$whisper_model" -f "$smoke_dir/whisper.wav" --no-prints 2>"$smoke_dir/whisper.stderr" || {
  cat "$smoke_dir/whisper.stderr" >&2
  exit 1
})
test -n "$(printf '%s' "$whisper_output" | tr -d '[:space:]')" || fail 'Whisper returned empty transcription'

normalize_words() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -cs '[:alnum:]' ' '
}

normalized_output=" $(normalize_words "$whisper_output") "
for word in $(normalize_words "$expected_text"); do
  case "$normalized_output" in
    *" $word "*) ;;
    *) fail "Whisper output did not contain expected word '$word': $(printf '%s' "$whisper_output" | tr '\n' ' ' | cut -c 1-240)" ;;
  esac
done

printf 'whisper_output=%s\n' "$(printf '%s' "$whisper_output" | tr '\n' ' ' | cut -c 1-240)"
echo 'real model smoke passed'
