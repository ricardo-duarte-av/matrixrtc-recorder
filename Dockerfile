# syntax=docker/dockerfile:1

# Build: CGO is needed for SQLite (mattn/go-sqlite3); Olm is pure Go (goolm).
FROM golang:1.26.9-alpine3.24 AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -tags goolm \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/matrixrtc-recorder ./cmd/matrixrtc-recorder

# Run: the bot keeps everything in /data (config.yaml, recorder.db, recordings/).
FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 1000 recorder \
 && mkdir /data && chown recorder:recorder /data
COPY --from=build /out/matrixrtc-recorder /usr/local/bin/matrixrtc-recorder
USER recorder
WORKDIR /data
VOLUME /data
ENTRYPOINT ["matrixrtc-recorder"]
CMD ["-config", "/data/config.yaml"]
