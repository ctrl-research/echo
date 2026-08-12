# syntax=docker/dockerfile:1

# ---- web client -------------------------------------------------------------
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# vite.config.ts writes to ../internal/webui/dist, so create the sibling path.
RUN mkdir -p /src/internal/webui && npm run build

# ---- server -----------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/internal/webui/dist internal/webui/dist

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -tags embedweb \
      -ldflags "-s -w \
        -X github.com/jonathanng/echo/internal/version.Version=${VERSION} \
        -X github.com/jonathanng/echo/internal/version.Commit=${COMMIT} \
        -X github.com/jonathanng/echo/internal/version.Date=${DATE}" \
      -o /out/echo ./cmd/echo

# ---- runtime ----------------------------------------------------------------
FROM alpine:3.22 AS runtime

# python3 is yt-dlp's runtime and ffmpeg does the audio conversion. mutagen is
# not optional despite looking like it: without it yt-dlp's --embed-thumbnail
# post-processor fails and the whole invocation exits non-zero, even though the
# audio was produced correctly.
RUN apk add --no-cache ca-certificates ffmpeg python3 py3-mutagen tzdata wget

# Pin YTDLP_VERSION for reproducible images. Left at "latest" by default
# because YouTube extraction breaks every few weeks and a stale pin fails
# closed; see docs/design.md, "Operational risks".
ARG YTDLP_VERSION=latest
RUN set -eux; \
    if [ "$YTDLP_VERSION" = "latest" ]; then \
      url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"; \
    else \
      url="https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp"; \
    fi; \
    wget -qO /usr/local/bin/yt-dlp "$url"; \
    chmod 0755 /usr/local/bin/yt-dlp; \
    /usr/local/bin/yt-dlp --version

RUN addgroup -g 10001 echo && adduser -D -u 10001 -G echo echo
COPY --from=build /out/echo /usr/local/bin/echo

# Declared so the image runs sanely without an explicit mount; compose and the
# k8s manifests override both with real volumes.
RUN mkdir -p /cache /library/downloads && chown -R echo:echo /cache /library
VOLUME ["/cache", "/library/downloads"]

USER echo:echo
EXPOSE 8080
ENV ECHO_ADDR=:8080 \
    ECHO_CACHE_DIR=/cache

ENTRYPOINT ["/usr/local/bin/echo"]
CMD ["serve"]
