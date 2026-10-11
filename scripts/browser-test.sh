#!/usr/bin/env bash
# Run Chromium against a fresh, offline app with disposable fixture data.
# Install tests/browser's locked dependencies and Chromium before invoking.
set -euo pipefail
umask 077

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d /tmp/voxmail-browser-ci.XXXXXX)
port=${VOXMAIL_BROWSER_PORT:-$(python3 - <<'PY'
import socket
with socket.socket() as listener:
    listener.bind(('127.0.0.1', 0))
    print(listener.getsockname()[1])
PY
)}
if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1024 || port > 65535 )); then
	echo "VOXMAIL_BROWSER_PORT must be between 1024 and 65535" >&2
	rm -rf "$work"
	exit 2
fi
server_pid=
cleanup() {
	status=$?
	trap - EXIT
	if [ -n "$server_pid" ]; then
		kill -TERM "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	if [ "$status" -ne 0 ] && [ -f "$work/server.log" ]; then
		cat "$work/server.log" >&2
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$root"
python3 - "$port" <<'PY'
import socket, sys
try:
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', int(sys.argv[1])))
except OSError as error:
    raise SystemExit('browser fixture port is already in use: ' + str(error))
PY
go build -o "$work/voxmail" "$root/cmd/voxmail"
"$root/tests/browser/prepare.sh" "$work/data"
env -i PATH="$PATH" \
	VOXMAIL_DATA_DIR="$work/data" \
	VOXMAIL_HTTP_ADDR="127.0.0.1:$port" \
	VOXMAIL_ENCRYPTION_KEY=voxmail-browser-disposable-key-0123456789abcdef \
	VOXMAIL_OFFLINE_MODE=1 \
	VOXMAIL_PIPER_BINARY="$work/no-piper" \
	"$work/voxmail" >"$work/server.log" 2>&1 &
server_pid=$!
ready=0
for ((attempt = 0; attempt < 100; attempt++)); do
	if ! kill -0 "$server_pid" 2>/dev/null; then
		echo "browser fixture server exited before readiness" >&2
		exit 1
	fi
	if curl --max-time 1 -fsS "http://127.0.0.1:$port/readyz" >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 0.2
done
if [ "$ready" != 1 ]; then
	echo "browser fixture server never became ready" >&2
	exit 1
fi
cd "$root/tests/browser"
VOXMAIL_BROWSER_BASE_URL="http://127.0.0.1:$port" \
	VOXMAIL_BROWSER_OUTPUT_DIR="${VOXMAIL_BROWSER_OUTPUT_DIR:-$root/tests/browser/test-results/${work##*/}}" \
	PLAYWRIGHT_HTML_OUTPUT_DIR="${PLAYWRIGHT_HTML_OUTPUT_DIR:-$root/tests/browser/playwright-report/${work##*/}}" npm test -- "$@"
