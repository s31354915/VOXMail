#!/bin/sh
# Verify that the operator-facing documentation still describes the checked-in
# deployment surface. This is intentionally textual: runtime behavior remains
# covered by the Go, native, browser, and opt-in integration suites.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

fail() {
	echo "documentation contract failed: $*" >&2
	exit 1
}

require_file() {
	test -f "$root/$1" || fail "missing file: $1"
}

contains() {
	file=$1
	needle=$2
	grep -F -- "$needle" "$root/$file" >/dev/null || fail "$file does not contain: $needle"
}

for file in \
	README.md \
	docs/ARCHITECTURE.md \
	docs/OPERATIONS.md \
	docs/RELEASE-READINESS.md \
	docs/SETTINGS.md \
	docs/WEB-CONSOLE.md \
	tests/browser/README.md; do
	require_file "$file"
done

require_file tests/browser/prepare.sh
test -x "$root/tests/browser/prepare.sh" || fail "tests/browser/prepare.sh is not executable"
contains tests/browser/README.md "tests/browser/prepare.sh"

# Every environment variable advertised in the example file must be findable
# in the settings reference. This catches documentation drift when a setting
# is added to the deployment surface.
while IFS= read -r line; do
	case "$line" in
		[A-Z_][A-Z0-9_]*=*)
			name=${line%%=*}
			contains docs/SETTINGS.md "$name"
			;;
	esac
done < "$root/.env.example"

for script in \
	scripts/backup.sh \
	scripts/restore-drill.sh \
	scripts/runtime-smoke.sh \
	scripts/real-model-smoke.sh \
	scripts/sip-peer-smoke.sh; do
	require_file "$script"
	test -x "$root/$script" || fail "$script is not executable"
	contains docs/OPERATIONS.md "$script"
done

contains README.md "docs/RELEASE-READINESS.md"
contains README.md "Beta software"
contains docs/SETTINGS.md "VOXMAIL_HTTP_ADDR"
contains docs/SETTINGS.md "127.0.0.1:8080"
contains docs/OPERATIONS.md "VOXMAIL_E2E_SIP_PEER=1 scripts/e2e.sh"
contains docs/RELEASE-READINESS.md "Promotion checklist"

echo "documentation contract passed"
