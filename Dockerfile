# syntax=docker/dockerfile:1
# Build/runtime image for VOXMail. Baresip, Piper, whisper.cpp, and mbsync are
# intentionally pinned by build arguments. BuildKit cache mounts keep the git
# sources and build trees across rebuilds so CI and local iterations are fast;
# the images themselves stay lean.
# Base manifest digests resolved 2026-10-06. Keep the human-readable tag next
# to the digest so refreshes can be reviewed as a deliberate distribution
# change; update both only through the documented image-pinning procedure.
FROM debian:bookworm@sha256:2c037a04925515fdd6ea85ea14a682d0e79931f5e9f5d07b6dbfc6ba12f9e858 AS baresip-build
# v4.11.0 tags resolved on 2026-09-24; callers may override with another
# immutable commit or a reviewed tag when intentionally updating the native stack.
ARG BARESIP_REF=3d30821f099925d24167f8a99e93ba4d1be98599
ARG RE_REF=ceefe9ff499aa1bcfb6255aff1737434dd385322
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git cmake make gcc g++ pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN --mount=type=cache,id=bare-re-source-${RE_REF}-${TARGETARCH},target=/src/re,sharing=locked \
    --mount=type=cache,id=bare-re-build-${RE_REF}-${TARGETARCH}-release,target=/build/re,sharing=locked \
    set -eux; \
    if [ ! -d /src/re/.git ]; then \
        git clone --filter=blob:none --no-checkout https://github.com/baresip/re.git /src/re; \
    fi; \
    git -C /src/re fetch --depth=1 origin "${RE_REF}"; \
    git -C /src/re checkout --detach FETCH_HEAD; \
    git -C /src/re clean -ffd; \
    cmake -S /src/re -B /build/re -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/re; \
    cmake --build /build/re --config Release -j"$(nproc)"; \
    cmake --install /build/re; \
    mkdir -p /out/provenance; \
    printf 'requested=%s\ncommit=%s\narchitecture=%s\n' "${RE_REF}" "$(git -C /src/re rev-parse HEAD)" "${TARGETARCH}" > /out/provenance/re.txt
COPY baresip/shim /src/appmodules/voxmail
RUN --mount=type=cache,id=bare-baresip-source-${BARESIP_REF}-${TARGETARCH},target=/src/baresip,sharing=locked \
    --mount=type=cache,id=bare-baresip-build-${BARESIP_REF}-${TARGETARCH}-release-modules-v1,target=/build/baresip,sharing=locked \
    set -eux; \
    if [ ! -d /src/baresip/.git ]; then \
        git clone --filter=blob:none --no-checkout https://github.com/baresip/baresip.git /src/baresip; \
    fi; \
    git -C /src/baresip fetch --depth=1 origin "${BARESIP_REF}"; \
    git -C /src/baresip checkout --detach FETCH_HEAD; \
    git -C /src/baresip clean -ffd; \
    cmake -S /src/baresip -B /build/baresip -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_PREFIX_PATH=/opt/re \
    -DMODULES='account;g711;auconv;auresamp;aubridge;aufile;in_band_dtmf;ice;stun;srtp;dtls_srtp;stdio' \
    -DAPP_MODULES_DIR=/src/appmodules -DAPP_MODULES=voxmail \
    -DVOXMAIL_BUILD_NATIVE_TESTS=ON; \
    cmake --build /build/baresip --target voxmail_command_queue_test voxmail_json_output_test voxmail_frame_parser_test voxmail_client_owner_test voxmail_command_wakeup_test voxmail_nonblocking_pipe_test voxmail_pcm_writer_test voxmail_pthread_start_test --config Release -j"$(nproc)"; \
    ctest --test-dir /build/baresip/app_modules/voxmail --output-on-failure; \
    cmake --build /build/baresip --config Release -j"$(nproc)"; \
    mkdir -p /out/modules /out/provenance; \
    cp /build/baresip/baresip /out/baresip; \
    cp /build/baresip/libbaresip.so /out/libbaresip.so; \
    find /build/baresip -name '*.so' -path '*/modules/*' -exec cp {} /out/modules/ \;; \
    find /build/baresip -name '*.so' -path '*/app_modules/*' -exec cp {} /out/modules/ \;; \
    printf 'requested=%s\ncommit=%s\narchitecture=%s\n' "${BARESIP_REF}" "$(git -C /src/baresip rev-parse HEAD)" "${TARGETARCH}" > /out/provenance/baresip.txt

FROM debian:bookworm@sha256:2c037a04925515fdd6ea85ea14a682d0e79931f5e9f5d07b6dbfc6ba12f9e858 AS whisper-build
# v1.7.1 tag resolved on 2026-09-24; callers may override with another
# immutable commit or a reviewed tag when intentionally updating whisper.cpp.
ARG WHISPER_REF=ebca09a3d1033417b0c630bbbe607b0f185b1488
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git cmake make gcc g++ pkg-config && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN --mount=type=cache,id=whisper-source-${WHISPER_REF}-${TARGETARCH},target=/src/whisper.cpp,sharing=locked \
    --mount=type=cache,id=whisper-build-${WHISPER_REF}-${TARGETARCH}-release-examples-native-off-v1,target=/build/whisper,sharing=locked \
    set -eux; \
    if [ ! -d /src/whisper.cpp/.git ]; then \
        git clone --filter=blob:none --no-checkout https://github.com/ggml-org/whisper.cpp.git /src/whisper.cpp; \
    fi; \
    git -C /src/whisper.cpp fetch --depth=1 origin "${WHISPER_REF}"; \
    git -C /src/whisper.cpp checkout --detach FETCH_HEAD; \
    git -C /src/whisper.cpp clean -ffd; \
    cmake -S /src/whisper.cpp -B /build/whisper -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DGGML_NATIVE=OFF; \
    cmake --build /build/whisper --config Release -j"$(nproc)" --target main; \
    mkdir -p /out /out/provenance; \
    cp /build/whisper/bin/main /out/whisper-cli; \
    if ldd /out/whisper-cli 2>&1 | grep -q 'not found'; then \
        echo 'whisper-cli has unresolved build-time dependencies' >&2; exit 1; \
    fi; \
    printf 'requested=%s\ncommit=%s\narchitecture=%s\n' "${WHISPER_REF}" "$(git -C /src/whisper.cpp rev-parse HEAD)" "${TARGETARCH}" > /out/provenance/whisper.txt

FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build
ARG TARGETARCH
ARG VOXMAIL_REVISION=unknown
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
RUN --mount=type=cache,id=gomod-${TARGETARCH},target=/go/pkg/mod,sharing=locked go mod download
COPY . .
RUN --mount=type=cache,id=gobuild-${TARGETARCH},target=/root/.cache/go-build,sharing=locked \
    set -eu; \
    CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/voxmail ./cmd/voxmail && \
    CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/voxmail-secret ./cmd/voxmail-secret && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/voxmail-connect ./cmd/voxmail-connect && \
    mkdir -p /out/provenance && \
    piper_model_sha="$(grep -m1 'PiperModelSHA256' internal/speech/provision.go | grep -oE '[0-9a-f]{64}')" && \
    piper_config_sha="$(grep -m1 'PiperConfigSHA256' internal/speech/provision.go | grep -oE '[0-9a-f]{64}')" && \
    whisper_model_sha="$(grep -m1 'WhisperBaseENSHA256' internal/speech/provision.go | grep -oE '[0-9a-f]{64}')" && \
    printf 'revision=%s\narchitecture=%s\n' "${VOXMAIL_REVISION}" "${TARGETARCH}" > /out/provenance/voxmail.txt && \
    printf 'piper_tts=1.3.0\npiper_model_sha256=%s\npiper_config_sha256=%s\nwhisper_model_sha256=%s\n' \
      "${piper_model_sha}" "${piper_config_sha}" "${whisper_model_sha}" > /out/provenance/models.txt

FROM python:3.11-slim-bookworm@sha256:0a310eeecf4e1f5a0743f9a6520c90c88d089c903ca5fd283f501e3a805f5f89b
ARG VOXMAIL_REVISION=unknown
LABEL org.opencontainers.image.revision="${VOXMAIL_REVISION}"
ENV VOXMAIL_DATA_DIR=/data \
    VOXMAIL_HTTP_ADDR=:8080 \
    VOXMAIL_MAX_CALLS=10
RUN apt-get update && apt-get upgrade -y --no-install-recommends && apt-get install -y --no-install-recommends \
    ca-certificates ffmpeg isync curl openssl libsqlite3-0 libssl3 libstdc++6 \
    && rm -rf /var/lib/apt/lists/* && pip install --no-cache-dir piper-tts==1.3.0 \
    && pip install --no-cache-dir --upgrade 'jaraco.context>=6.1.0' 'wheel>=0.46.2'
COPY --from=build /out/voxmail /usr/local/bin/voxmail
COPY --from=build /out/voxmail-secret /usr/local/bin/voxmail-secret
COPY --from=build /out/voxmail-connect /usr/local/bin/voxmail-connect
COPY --from=baresip-build /out/baresip /usr/local/bin/baresip
COPY --from=baresip-build /opt/re/lib /usr/local/lib
COPY --from=baresip-build /out/libbaresip.so /usr/local/lib/libbaresip.so
COPY --from=baresip-build /out/modules /usr/local/lib/baresip/modules
COPY --from=whisper-build /out/whisper-cli /usr/local/bin/whisper-cli
COPY --from=build /out/provenance /usr/local/share/voxmail/provenance
COPY --from=baresip-build /out/provenance /usr/local/share/voxmail/provenance
COPY --from=whisper-build /out/provenance /usr/local/share/voxmail/provenance
COPY assets/welcome.wav /usr/local/share/voxmail/welcome.wav
COPY assets/main-menu.wav /usr/local/share/voxmail/main-menu.wav
COPY assets/static-prompts.json /usr/local/share/voxmail/static-prompts.json
COPY scripts/entrypoint.sh /usr/local/bin/voxmail-entrypoint
COPY scripts/runtime-smoke.sh /usr/local/bin/voxmail-runtime-smoke
COPY scripts/real-model-smoke.sh /usr/local/bin/voxmail-real-model-smoke
COPY scripts/sip-peer-smoke.sh /usr/local/bin/voxmail-sip-peer-smoke
COPY tests/sippeer /usr/local/lib/voxmail/sippeer
RUN set -eu; \
    find /usr/local/lib/baresip -type d -exec chmod 0755 {} +; \
    find /usr/local/lib/baresip -type f -exec chmod 0755 {} +; \
    find /usr/local/share/voxmail -type d -exec chmod 0755 {} +; \
    find /usr/local/share/voxmail -type f -exec chmod 0644 {} +; \
    chmod 0755 /usr/local/bin/voxmail /usr/local/bin/voxmail-secret /usr/local/bin/voxmail-connect /usr/local/bin/baresip /usr/local/bin/whisper-cli /usr/local/lib/libbaresip.so; \
    find /usr/local/lib -maxdepth 1 -type f -name 'libre.so*' -exec chmod 0755 {} +; \
    ldconfig; \
    for target in /usr/local/bin/baresip /usr/local/bin/whisper-cli /usr/local/lib/libbaresip.so /usr/local/lib/baresip/modules/*.so; do \
        if ldd "$target" 2>&1 | grep -q 'not found'; then \
            echo "unresolved runtime dependency: $target" >&2; exit 1; \
        fi; \
    done; \
    chmod 0755 /usr/local/bin/voxmail-entrypoint /usr/local/bin/voxmail-runtime-smoke /usr/local/bin/voxmail-real-model-smoke /usr/local/bin/voxmail-sip-peer-smoke /usr/local/bin/voxmail-connect && \
    useradd --system --uid 10001 --no-create-home voxmail && \
    mkdir -p /data /data/logs /data/run/voxmail && \
    chown -R voxmail:voxmail /data
USER voxmail
VOLUME ["/data"]
EXPOSE 5060/udp 5060/tcp 8080/tcp
ENTRYPOINT ["/usr/local/bin/voxmail-entrypoint"]
