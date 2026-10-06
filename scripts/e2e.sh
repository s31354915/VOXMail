#!/usr/bin/env bash
# End-to-end tests for VOXMail: build the image, run a throwaway
# container, hammer it over HTTP, restart it to prove state persistence, then
# remove every trace (container, volume, image, the temp dir) on the way out.
#
# Usage:  scripts/e2e.sh (optionally VOXMAIL_E2E_WEB_PORT=18080)
#
# The default run is API-only and does not manufacture fake speech models.
# Set VOXMAIL_E2E_CALLS=1 and provide real model paths to run the separate
# speech/SIP acceptance path.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${VOXMAIL_E2E_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}"
case "$RUN_ID" in
  ''|*[!A-Za-z0-9_.-]*) echo "invalid VOXMAIL_E2E_RUN_ID" >&2; exit 2 ;;
esac
WEBPORT="${VOXMAIL_E2E_WEB_PORT:-}"
CALLS="${VOXMAIL_E2E_CALLS:-0}"
case "$CALLS" in
  0|1) ;;
  *) echo "VOXMAIL_E2E_CALLS must be 0 or 1" >&2; exit 2 ;;
esac
SIP_PEER="${VOXMAIL_E2E_SIP_PEER:-0}"
case "$SIP_PEER" in
  0|1) ;;
  *) echo "VOXMAIL_E2E_SIP_PEER must be 0 or 1" >&2; exit 2 ;;
esac
IMAGE="voxmail-e2e:${RUN_ID}"
NAME="voxmail-e2e-${RUN_ID}"
VOLUME="voxmail-e2e-data-${RUN_ID}"
WORK="$(mktemp -d /tmp/voxmail-e2e.XXXXXX)"
ARTIFACT_DIR="${VOXMAIL_E2E_ARTIFACT_DIR:-$ROOT/.e2e-artifacts/$RUN_ID}"

DOCKER_MODEL_ARGS=()
DOCKER_MODEL_ENV=()
if [ "$CALLS" = 1 ]; then
  PIPER_HOST="${VOXMAIL_E2E_PIPER_MODEL:-}"
  WHISPER_HOST="${VOXMAIL_E2E_WHISPER_MODEL:-}"
  WHISPER_FIXTURE_HOST="${VOXMAIL_E2E_WHISPER_FIXTURE:-}"
  test -s "$PIPER_HOST" || { echo "VOXMAIL_E2E_PIPER_MODEL must point to a real Piper model" >&2; exit 2; }
  test -s "${PIPER_HOST}.json" || { echo "Piper config must be beside the model as ${PIPER_HOST}.json" >&2; exit 2; }
  test -s "$WHISPER_HOST" || { echo "VOXMAIL_E2E_WHISPER_MODEL must point to a real Whisper model" >&2; exit 2; }
  if [ -n "$WHISPER_FIXTURE_HOST" ]; then
    test -s "$WHISPER_FIXTURE_HOST" || { echo "VOXMAIL_E2E_WHISPER_FIXTURE must point to a real audio fixture" >&2; exit 2; }
  fi
  DOCKER_MODEL_ARGS+=(
    -v "$PIPER_HOST:/e2e/piper/model.onnx:ro"
    -v "${PIPER_HOST}.json:/e2e/piper/model.onnx.json:ro"
    -v "$WHISPER_HOST:/e2e/whisper/model.bin:ro"
  )
  DOCKER_MODEL_ENV+=(
    -e VOXMAIL_PIPER_MODEL=/e2e/piper/model.onnx
    -e VOXMAIL_STT_MODEL=/e2e/whisper/model.bin
  )
  if [ -n "$WHISPER_FIXTURE_HOST" ]; then
    DOCKER_MODEL_ARGS+=(-v "$WHISPER_FIXTURE_HOST:/e2e/whisper/fixture.wav:ro")
    DOCKER_MODEL_ENV+=(-e VOXMAIL_WHISPER_FIXTURE=/e2e/whisper/fixture.wav)
  fi
fi

if [ -n "$WEBPORT" ]; then
  PORT_ARGS=(-p "127.0.0.1:$WEBPORT:8080/tcp")
else
  PORT_ARGS=(-p "127.0.0.1::8080/tcp")
fi

cleanup() {
  status=$?
  echo "== cleaning up (run $RUN_ID)"
  set +e
  if [ "$status" -ne 0 ]; then
    mkdir -p "$ARTIFACT_DIR"
    docker logs "$NAME" 2>&1 | sed -E 's/(password|token|secret|encryption_key)[=:][^[:space:]]+/\1=[REDACTED]/Ig' >"$ARTIFACT_DIR/container.log"
    cp "$WORK/docker-run.log" "$ARTIFACT_DIR/docker-run.log" 2>/dev/null || true
    echo "diagnostics saved to $ARTIFACT_DIR" >&2
  fi
  docker rm -f "$NAME" >/dev/null 2>&1
  docker volume rm -f "$VOLUME" >/dev/null 2>&1
  docker image rm -f "$IMAGE" >/dev/null 2>&1
  rm -rf -- "$WORK"
  exit "$status"
}
trap cleanup EXIT

echo "== building image $IMAGE (this clones baresip/re/whisper.cpp and compiles)"
docker build --progress=plain -t "$IMAGE" "$ROOT"

if [ "$SIP_PEER" = 1 ]; then
  echo "== opt-in local SIP/RTP peer smoke test"
  docker run --rm --entrypoint /usr/local/bin/voxmail-sip-peer-smoke "$IMAGE"
fi

docker volume create --label "com.voxmail.e2e.run=$RUN_ID" "$VOLUME" >/dev/null
echo "== starting container $NAME"
docker run -d --name "$NAME" \
  -e VOXMAIL_ENCRYPTION_KEY="voxmail-e2e-encryption-key-0123456789abcdef" \
  -e VOXMAIL_ENABLE_CALLS="$CALLS" \
  -e VOXMAIL_E2E_CALLS="$CALLS" \
  -e VOXMAIL_PROVISION_MODELS=0 \
  "${PORT_ARGS[@]}" \
  "${DOCKER_MODEL_ENV[@]}" \
  "${DOCKER_MODEL_ARGS[@]}" \
  -v "$VOLUME:/data" \
  --label "com.voxmail.e2e.run=$RUN_ID" \
  "$IMAGE" >>"$WORK/docker-run.log" 2>&1

if [ -z "$WEBPORT" ]; then
  mapped_port="$(docker port "$NAME" 8080/tcp | sed -n '1{s/.*://p}')"
  test -n "$mapped_port" || { echo "could not discover the mapped web port" >&2; exit 1; }
  WEBPORT="$mapped_port"
fi
echo "web console mapped to 127.0.0.1:$WEBPORT"

wait_ready() {
  for _ in $(seq 1 90); do
    if curl --max-time 3 -fsS "http://127.0.0.1:$WEBPORT/readyz" >/dev/null 2>&1; then
      if [ "$CALLS" = 0 ] || docker exec "$NAME" test -S /data/run/baresip.sock; then
        return 0
      fi
    fi
    sleep 2
  done
  echo "container never became ready; last logs:" >&2
  docker logs "$NAME" 2>&1 | tail -40 || true
  return 1
}
wait_ready

echo "== phase 1: API tests"
VOXMAIL_URL="http://127.0.0.1:$WEBPORT" go run "$ROOT/tests/e2e"

echo "== phase 1.5: container internals"
for probe in \
  "/data/sqlite/voxmail.db" \
  "/data/prompts/welcome.wav" \
  "/data/prompts/main-menu.wav"; do
  if ! docker exec "$NAME" test -s "$probe"; then
    echo "missing internal file: $probe" >&2
    exit 1
  fi
  echo "  ok   internal file $probe"
done
for binary in /usr/local/bin/baresip /usr/local/bin/voxmail /usr/local/bin/whisper-cli; do
  if ! docker exec "$NAME" test -s "$binary"; then
    echo "missing binary: $binary" >&2
    exit 1
  fi
  echo "  ok   binary $binary"
done
if [ "$CALLS" = 1 ]; then
  if ! docker exec "$NAME" test -s /data/logs/baresip.log; then
    echo "missing baresip log" >&2
    exit 1
  fi
  if ! docker exec "$NAME" test -S /data/run/baresip.sock; then
    echo "missing baresip control socket" >&2
    exit 1
  fi
  echo "  ok   baresip socket"
  SMOKE_ENV=(
    env
    VOXMAIL_PIPER_MODEL=/e2e/piper/model.onnx
    VOXMAIL_WHISPER_MODEL=/e2e/whisper/model.bin
  )
  if [ -n "${VOXMAIL_E2E_WHISPER_FIXTURE:-}" ]; then
    SMOKE_ENV+=(VOXMAIL_WHISPER_FIXTURE=/e2e/whisper/fixture.wav)
  fi
  docker exec "$NAME" "${SMOKE_ENV[@]}" /usr/local/bin/voxmail-runtime-smoke
  docker exec "$NAME" "${SMOKE_ENV[@]}" /usr/local/bin/voxmail-real-model-smoke
fi

echo "== phase 2: restart the container and verify persistence"
docker restart "$NAME" >/dev/null
wait_ready
VOXMAIL_URL="http://127.0.0.1:$WEBPORT" go run "$ROOT/tests/e2e" -after-restart

echo "== phase 3: connectivity and open ports"
curl --max-time 5 -fsS "http://127.0.0.1:$WEBPORT/healthz" >/dev/null
echo "  ok   healthz reachable"
curl --max-time 5 -fsS "http://127.0.0.1:$WEBPORT/readyz" >/dev/null
echo "  ok   readyz reachable"
if [ "$CALLS" = 1 ]; then
  for expected in \
    'sip_listen 0.0.0.0:5060' \
    'call_accept yes' \
    'audio_source voxmail'; do
    if ! docker exec "$NAME" grep -q "$expected" /data/config/baresip/config; then
      echo "baresip config is missing: $expected" >&2
      exit 1
    fi
    echo "  ok   baresip config: $expected"
  done
fi

echo
echo "E2E suite green."
