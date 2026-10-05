# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.5
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=build_ca_bundle \
    if [ -f /run/secrets/build_ca_bundle ]; then export SSL_CERT_FILE=/run/secrets/build_ca_bundle; fi; \
    go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=secret,id=build_ca_bundle \
    if [ -f /run/secrets/build_ca_bundle ]; then export SSL_CERT_FILE=/run/secrets/build_ca_bundle; fi; \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/agenticgo ./cmd/agenticgo

FROM debian:bookworm-slim

LABEL org.opencontainers.image.title="agenticgo" \
      org.opencontainers.image.description="Self-hosted AI agent gateway" \
      org.opencontainers.image.source="https://github.com/dkr290/agenticgo"

# Standard exec commands need bubblewrap and their usual Linux executables.
# Add any dangerous extra commands in a derived image (see README).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bubblewrap ca-certificates coreutils curl findutils grep tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 agenticgo \
    && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin agenticgo \
    && mkdir /data \
    && chown 10001:10001 /data

COPY --from=build /out/agenticgo /usr/local/bin/agenticgo

ENV AGENTICGO_ADDR=:8080 \
    AGENTICGO_DATA_DIR=/data \
    HOME=/home/agenticgo \
    TZ=Etc/UTC
WORKDIR /data
USER 10001:10001
VOLUME ["/data"]
EXPOSE 8080
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD curl --fail --silent --show-error --max-time 2 "http://127.0.0.1:${AGENTICGO_ADDR##*:}/healthz" || exit 1

# The Go process receives container signals directly and handles graceful shutdown.
ENTRYPOINT ["/usr/local/bin/agenticgo"]
