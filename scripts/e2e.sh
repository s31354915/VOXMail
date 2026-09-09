#!/usr/bin/env bash
# End-to-end "killer" tests for VOXMail: build the image, run a throwaway
# container, hammer it over HTTP, restart it to prove state persistence, then
# remove every trace (container, volume, image, the temp dir) on the way out.
#
# Usage:  scripts/e2e.sh   (optionally VOXMAIL_E2E_WEB_PORT=18080)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEBPORT="${VOXMAIL_E2E_WEB_PORT:-18080}"
IMAGE="voxmail-e2e:test"
NAME="voxmail-e2e"
VOLUME="voxmail-e2e-data"
WORK="$(mktemp -d /tmp/voxmail-e2e.XXXXXX)"

cleanup() {
  echo "== cleaning up (containers, volumes, images, temp dirs)"
  set +e
  docker rm -f "$NAME" >/dev/null 2>&1
  docker volume rm -f "$VOLUME" >/dev/null 2>&1
  docker image rm -f "$IMAGE" >/dev/null 2>&1
  docker container prune -f >/dev/null 2>&1
  docker volume prune -f >/dev/null 2>&1
  docker image prune -f >/dev/null 2>&1
  docker system prune -f >/dev/null 2>&1
  rm -rf -- "$WORK"
  set -e
}
trap cleanup EXIT

echo "== building image $IMAGE (this clones baresip/re/whisper.cpp and compiles)"
docker build --progress=plain -t "$IMAGE" "$ROOT"

echo "== starting container $NAME on 127.0.0.1:$WEBPORT"
docker run -d --name "$NAME" \
  -e VOXMAIL_ENCRYPTION_KEY="voxmail-e2e-encryption-key-0123456789abcdef" \
  -e VOXMAIL_ENABLE_CALLS=1 \
  -e VOXMAIL_PROVISION_MODELS=0 \
  -p "127.0.0.1:$WEBPORT:8080/tcp" \
  -v "$VOLUME:/data" \
  "$IMAGE" >>"$WORK/docker-run.log" 2>&1

wait_ready() {
  for _ in $(seq 1 90); do
    if docker exec "$NAME" sh -c 'test -S /data/run/baresip.sock' 2>/dev/null \
      && curl -fsS "http://127.0.0.1:$WEBPORT/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "container never became ready; last logs:" >&2
  docker logs "$NAME" 2>&1 | tail -40 || true
  return 1
}
wait_ready

echo "== phase 1: API killer tests"
VOXMAIL_URL="http://127.0.0.1:$WEBPORT" go run "$ROOT/tests/e2e"

echo "== phase 1.5: container internals"
for probe in \
  "/data/sqlite/voxmail.db" \
  "/data/logs/baresip.log" \
  "/data/run/baresip.sock" \
  "/data/prompts/welcome.wav" \
  "/data/prompts/main-menu.wav"; do
  docker exec "$NAME" test -s "$probe" && echo "  ok   internal file $probe"
done
docker exec "$NAME" test -s /usr/local/bin/baresip && echo "  ok   baresip binary"
docker exec "$NAME" test -s /usr/local/bin/voxmail && echo "  ok   voxmail binary"
docker exec "$NAME" test -s /usr/local/bin/whisper-cli && echo "  ok   whisper-cli binary"

echo "== phase 2: restart the container and verify persistence"
docker restart "$NAME" >/dev/null
wait_ready
VOXMAIL_URL="http://127.0.0.1:$WEBPORT" go run "$ROOT/tests/e2e" -after-restart

echo "== phase 3: connectivity and open ports"
curl -fsS "http://127.0.0.1:$WEBPORT/healthz" >/dev/null && echo "  ok   healthz reachable"
curl -fsS "http://127.0.0.1:$WEBPORT/readyz" >/dev/null && echo "  ok   readyz reachable"
docker exec "$NAME" sh -c 'grep -q "sip_listen 0.0.0.0:5060" /data/config/baresip/config' && echo "  ok   baresip listens on SIP 5060"
docker exec "$NAME" sh -c 'grep -q "call_accept yes" /data/config/baresip/config' && echo "  ok   inbound calls accepted"
docker exec "$NAME" sh -c 'grep -q "audio_source voxmail" /data/config/baresip/config' && echo "  ok   voxmail audio source pinned"

echo
echo "E2E suite green."