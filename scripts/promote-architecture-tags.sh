#!/usr/bin/env bash
set -euo pipefail

scan_result=${1:-}
image=${2:?image reference is required}
tag=${3:?release tag is required}
digest_dir=${4:?digest directory is required}

if [[ "$scan_result" != success ]]; then
	echo "refusing architecture alias promotion: scan result is ${scan_result:-missing}" >&2
	exit 1
fi

manifest_digest=${5:?manifest digest is required}
[[ "$tag" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,121}$ ]]
[[ "$manifest_digest" =~ ^sha256:[0-9a-f]{64}$ ]]
declare -A desired
for arch in amd64 arm64; do
	digest="$(cat "$digest_dir/$arch/digest")"
	[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
	desired["${tag}-${arch}"]=$digest
done
desired["$tag"]=$manifest_digest

# All non-latest aliases are immutable. Check every target before any public
# write so an existing conflicting manifest cannot leave new architecture tags.
# A failed tag listing or digest lookup is an error, never proof of absence.
if [[ "$tag" != latest ]]; then
	existing_tags="$(crane ls "$image")"
	for alias in "${tag}-amd64" "${tag}-arm64" "$tag"; do
		if [[ $'\n'"$existing_tags"$'\n' == *$'\n'"$alias"$'\n'* ]]; then
			current="$(crane digest "${image}:${alias}")"
			if [[ "$current" != "${desired[$alias]}" ]]; then
				echo "refusing to overwrite immutable release alias ${image}:${alias}: $current != ${desired[$alias]}" >&2
				exit 1
			fi
		fi
	done
fi

for alias in "${tag}-amd64" "${tag}-arm64" "$tag"; do
	crane tag "${image}@${desired[$alias]}" "$alias"
	promoted="$(crane digest "${image}:${alias}")"
	[[ "$promoted" == "${desired[$alias]}" ]]
done
