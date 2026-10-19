# syntax=docker/dockerfile:1
# Multi-stage: golang:alpine builder -> alpine runner.
# Produces a tiny static binary (~8-12 MB) running at <30 MB RSS.
ARG GO_VERSION=1.22

FROM golang:${GO_VERSION}-alpine AS builder
# TARGETARCH is supplied by buildx (amd64/arm64/...). Never hardcode GOARCH:
# a forced arch mismatching the image arch fails at runtime with
# "exec format error". When unset (plain docker build), Go defaults to the
# builder's arch, which always matches the target image.
ARG TARGETARCH
WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata
COPY go.mod ./
# stdlib-only: no downloads, but keep the layer warm for future deps.
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -extldflags '-static'" \
    -o /out/musicstreamer .

FROM alpine:latest
# yt-dlp resolves YouTube audio; nodejs is its JS runtime for YouTube's
# challenges (Alpine's node is >=22, injected as --js-runtimes node at runtime).
RUN apk add --no-cache ca-certificates tzdata yt-dlp nodejs \
    && addgroup -S app && adduser -S app -G app \
    && mkdir -p /data/cache && chown -R app:app /data
COPY --from=builder /out/musicstreamer /usr/local/bin/musicstreamer
# FUSE/audio device access is granted at runtime via --device/--group-add;
# no extra packages needed for pure HTTP streaming.
USER app
VOLUME ["/data/cache"]
EXPOSE 4533
ENV PORT=4533 CACHE_DIR=/data/cache CACHE_SIZE_GB=10
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:${PORT:-4533}/healthz || exit 1
ENTRYPOINT ["musicstreamer"]
