# Container build for running the MCP server via Docker (e.g. Docker Desktop
# on macOS). Stdio transport: run with -i, no ports. DB lives in /data —
# mount a volume to keep parts/specs/listings across sessions.
# --platform=$BUILDPLATFORM + GOOS/GOARCH cross-compile: the Go build always
# runs natively (fast) even when producing the arm64 image on an amd64 runner.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev TARGETOS TARGETARCH
# CGO off: sqlite driver is pure Go (modernc.org/sqlite).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /parts-finder .

FROM debian:bookworm-slim
# ca-certificates: https everywhere. The lightpanda renderer is auto-downloaded
# to ~/.cache on first bot-walled fetch; mount /root/.cache to keep it.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /parts-finder /usr/local/bin/parts-finder
ENV PARTS_DB=/data/parts-finder.db
VOLUME /data
ENTRYPOINT ["parts-finder"]
