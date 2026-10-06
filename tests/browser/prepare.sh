#!/bin/sh
# Prepare only the metadata fixture needed by browser UI tests. The model file
# is deliberately not valid Piper audio/model data and must never be used by a
# speech or runtime smoke test.
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 /tmp/voxmail-browser-data-XXXXXX" >&2
	exit 2
fi

data_root=$1
case "$data_root" in
	/tmp/voxmail-browser-*) : ;;
	*) echo "refusing unsafe browser fixture path: $data_root" >&2; exit 2 ;;
esac

umask 077
voice_dir="$data_root/voices"
mkdir -p "$voice_dir"
printf '%s\n' 'VOXMail browser-only metadata fixture; never pass this to Piper.' > "$voice_dir/en_US-hfc_male-medium.onnx"
printf '%s\n' '{"browser_fixture":true}' > "$voice_dir/en_US-hfc_male-medium.onnx.json"
echo "browser voice metadata fixture prepared under $voice_dir"
