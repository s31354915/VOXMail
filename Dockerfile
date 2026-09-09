# Build/runtime image for VOXMail. Baresip, Piper, whisper.cpp, and mbsync are
# intentionally pinned by build arguments. BuildKit cache mounts keep the git
# sources and build trees across rebuilds so CI and local iterations are fast;
# the images themselves stay lean.
FROM debian:bookworm AS baresip-build
ARG BARESIP_REF=v4.11.0
ARG RE_REF=v4.11.0
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git cmake make gcc g++ pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN --mount=type=cache,id=bare-re,target=/src/re \
    { test -d /src/re/.git || git clone --depth 1 --branch ${RE_REF} https://github.com/baresip/re.git /src/re; } \
    && cmake -S /src/re -B /src/re/build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/re \
    && cmake --build /src/re/build --config Release -j$(nproc) \
    && cmake --install /src/re/build
RUN --mount=type=cache,id=bare-baresip,target=/src/baresip \
    { test -d /src/baresip/.git || git clone --depth 1 --branch ${BARESIP_REF} https://github.com/baresip/baresip.git /src/baresip; }
COPY baresip/shim /src/appmodules/voxmail
RUN --mount=type=cache,id=bare-baresip,target=/src/baresip \
    cmake -S /src/baresip -B /src/baresip/build -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_PREFIX_PATH=/opt/re \
    -DMODULES='account;g711;auconv;auresamp;aubridge;aufile;in_band_dtmf;ice;stun;srtp;dtls_srtp;stdio' \
    -DAPP_MODULES_DIR=/src/appmodules -DAPP_MODULES=voxmail \
    && cmake --build /src/baresip/build --config Release -j$(nproc)
RUN --mount=type=cache,id=bare-baresip,target=/src/baresip \
    mkdir -p /out/modules && cp /src/baresip/build/baresip /out/baresip && \
    cp /src/baresip/build/libbaresip.so /out/libbaresip.so && \
    find /src/baresip/build -name '*.so' -path '*/modules/*' -exec cp {} /out/modules/ \; && \
    find /src/baresip/build -name '*.so' -path '*/app_modules/*' -exec cp {} /out/modules/ \;

FROM debian:bookworm AS whisper-build
ARG WHISPER_REF=v1.7.1
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git cmake make gcc g++ pkg-config && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN --mount=type=cache,id=whisper-src,target=/src/whisper.cpp \
    { test -d /src/whisper.cpp/.git || git clone --depth 1 --branch ${WHISPER_REF} https://github.com/ggml-org/whisper.cpp.git /src/whisper.cpp; } \
    && cmake -S /src/whisper.cpp -B /src/whisper.cpp/build -DCMAKE_BUILD_TYPE=Release -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DGGML_NATIVE=OFF \
    && cmake --build /src/whisper.cpp/build --config Release -j$(nproc) --target main \
    && mkdir -p /out && cp /src/whisper.cpp/build/bin/main /out/whisper-cli

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/voxmail ./cmd/voxmail && \
    CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/voxmail-secret ./cmd/voxmail-secret

FROM python:3.11-slim
ENV VOXMAIL_DATA_DIR=/data \
    VOXMAIL_HTTP_ADDR=:8080 \
    VOXMAIL_MAX_CALLS=10
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates ffmpeg isync curl openssl libsqlite3-0 libssl3 \
    && rm -rf /var/lib/apt/lists/* && pip install --no-cache-dir piper-tts==1.3.0
COPY --from=build /out/voxmail /usr/local/bin/voxmail
COPY --from=build /out/voxmail-secret /usr/local/bin/voxmail-secret
COPY --from=baresip-build /out/baresip /usr/local/bin/baresip
COPY --from=baresip-build /opt/re/lib /usr/local/lib
COPY --from=baresip-build /out/libbaresip.so /usr/local/lib/libbaresip.so
COPY --from=baresip-build /out/modules /usr/local/lib/baresip/modules
COPY --from=whisper-build /out/whisper-cli /usr/local/bin/whisper-cli
COPY assets/welcome.wav /usr/local/share/voxmail/welcome.wav
COPY assets/main-menu.wav /usr/local/share/voxmail/main-menu.wav
COPY assets/static-prompts.json /usr/local/share/voxmail/static-prompts.json
COPY scripts/entrypoint.sh /usr/local/bin/voxmail-entrypoint
RUN chmod 0755 /usr/local/bin/voxmail-entrypoint && \
    useradd --system --uid 10001 --no-create-home voxmail && \
    mkdir -p /data /data/logs /data/run/voxmail && \
    chown -R voxmail:voxmail /data && \
    ldconfig
USER voxmail
VOLUME ["/data"]
EXPOSE 5060/udp 5060/tcp 8080/tcp
ENTRYPOINT ["/usr/local/bin/voxmail-entrypoint"]