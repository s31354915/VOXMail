# Build/runtime image for the first production slice. Baresip, Piper,
# whisper.cpp, and mbsync are intentionally pinned by build arguments.
FROM debian:bookworm AS baresip-build
ARG BARESIP_REF=v4.11.0
ARG RE_REF=main
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git cmake make gcc g++ pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 --branch ${RE_REF} https://github.com/baresip/re.git re \
    && cmake -S re -B re/build -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/re \
    && cmake --build re/build -j2 && cmake --install re/build
RUN git clone --depth 1 --branch ${BARESIP_REF} https://github.com/baresip/baresip.git baresip
COPY baresip/shim /src/appmodules/voxmail
RUN cmake -S baresip -B baresip/build -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_PREFIX_PATH=/opt/re \
    -DMODULES='account;g711;auconv;auresamp;aubridge;aufile;in_band_dtmf;ice;stun;srtp;dtls_srtp;stdio' \
    -DAPP_MODULES_DIR=/src/appmodules -DAPP_MODULES=voxmail \
    && cmake --build baresip/build -j2
RUN mkdir -p /out/modules && cp baresip/build/baresip /out/baresip && \
    cp baresip/build/libbaresip.so /out/libbaresip.so && \
    find baresip/build -name '*.so' -path '*/modules/*' -exec cp {} /out/modules/ \; && \
    find baresip/build -name '*.so' -path '*/app_modules/*' -exec cp {} /out/modules/ \;

FROM debian:bookworm AS whisper-build
ARG WHISPER_REF=v1.7.1
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git cmake make gcc g++ pkg-config && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 --branch ${WHISPER_REF} https://github.com/ggml-org/whisper.cpp.git whisper.cpp && \
    cmake -S whisper.cpp -B whisper.cpp/build -DCMAKE_BUILD_TYPE=Release -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DGGML_NATIVE=OFF && \
    cmake --build whisper.cpp/build --config Release -j2 --target main && \
    mkdir -p /out && cp whisper.cpp/build/bin/main /out/whisper-cli

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/voxmail ./cmd/voxmail && \
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
COPY scripts/entrypoint.sh /usr/local/bin/voxmail-entrypoint
RUN chmod 0755 /usr/local/bin/voxmail-entrypoint && mkdir -p /data /data/logs && ldconfig
VOLUME ["/data"]
EXPOSE 8080/udp 8080/tcp
ENTRYPOINT ["/usr/local/bin/voxmail-entrypoint"]
