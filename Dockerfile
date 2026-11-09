# syntax=docker/dockerfile:1

# --- Stage 1: static Go binary (HTML/CSS/JS embedded) -------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go config.default.json ./
COPY static ./static
COPY templates ./templates

# The build context has no .git, so deploy-k3s.sh passes the version in.
ARG VERSION=docker
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.appVersion=${VERSION}" -o /out/leecher .

# --- Stage 2: helper binaries -------------------------------------------------
# Static ffmpeg/ffprobe for merging and remuxing; deno is the JS runtime yt-dlp
# needs for YouTube. yt-dlp itself is fetched (checksum-verified) at first start
# into the data volume so it can self-update without rebuilding the image.
FROM mwader/static-ffmpeg:9.0.2 AS ffmpeg
FROM denoland/deno:bin-2.9.8 AS deno

# --- Stage 3: runtime ---------------------------------------------------------
# glibc base: the standalone yt-dlp and deno builds are not musl-compatible.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 1000 leecher \
 && useradd --uid 1000 --gid 1000 --home-dir /data --no-create-home --shell /usr/sbin/nologin leecher \
 && mkdir -p /data && chown 1000:1000 /data

COPY --from=ffmpeg /ffmpeg /ffprobe /usr/local/bin/
COPY --from=deno /deno /usr/local/bin/deno
COPY --from=build /out/leecher /app/leecher
COPY LICENSE LEGAL.md THIRD_PARTY_NOTICES.md /app/
COPY third_party /app/third_party
COPY static/fonts/OFL.txt /app/static/fonts/OFL.txt

# Everything mutable (config.json, state/, bin/yt-dlp, .cache, downloads/)
# lives under LEECHER_HOME on the mounted volume.
ENV LEECHER_HOME=/data
# Plain reads instead of mmap for torrent data: page faults on a slow USB disk
# would otherwise stall the whole Go process (see ensureClassicTorrentIO).
ENV TORRENT_STORAGE_DEFAULT_FILE_IO=classic
WORKDIR /data
# UID/GID 1000 matches Transmission's PUID/PGID so shared files line up.
USER 1000:1000
EXPOSE 9090
ENTRYPOINT ["/app/leecher"]
