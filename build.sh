#!/bin/bash
# build.sh — compile leecher for Linux, macOS, and Windows.
#
# Cross-compiles from any OS with Go installed. No C compiler needed.
#
# Usage:
#   bash build.sh                    # build for current OS/arch
#   bash build.sh all                # build for all platforms
#   bash build.sh linux              # build all Linux targets
#   bash build.sh linux/arm64        # build a specific target
#
# Outputs:
#   dist/leecher-<os>-<arch>/
#     ├── leecher              # single binary (all assets embedded)
#     ├── config.json.example
#     ├── install.sh
#     ├── uninstall.sh
#     ├── run.sh
#     └── leecher.service

set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

APP_NAME="leecher"

echo ""
echo "  ╔══════════════════════════════════════════╗"
echo "  ║           leecher build (Go)             ║"
echo "  ╚══════════════════════════════════════════╝"
echo ""
echo "  source  : $DIR"
echo ""

# Resolve dependencies
echo "[1/3] Resolving Go modules..."
go mod tidy

# Determine version from git tag or date. This is a monorepo, so prefer tags
# scoped to this project — a repo-wide tag would move whenever a sibling
# project is released.
# Note: plain `[[ ... ]] && x=y` would abort the script under `set -e` whenever
# the test is false.
VERSION=$(git describe --tags --match 'leecher/v*' --dirty 2>/dev/null | sed 's|^leecher/||' || true)
if [[ -z "$VERSION" ]]; then
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || true)
fi
if [[ -z "$VERSION" ]]; then
    VERSION=$(date +%Y%m%d)
fi
echo "  version : $VERSION"
echo ""

# All supported platforms
ALL_PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "linux/arm"
    "darwin/amd64"
    "darwin/arm64"
    "windows/amd64"
)

# Determine target platforms from argument
TARGET="${1:-current}"
PLATFORMS=()

case "$TARGET" in
  all)
    PLATFORMS=("${ALL_PLATFORMS[@]}")
    ;;
  current)
    PLATFORMS=("$(go env GOOS)/$(go env GOARCH)")
    ;;
  linux)
    for p in "${ALL_PLATFORMS[@]}"; do
      [[ "$p" == linux/* ]] && PLATFORMS+=("$p")
    done
    ;;
  darwin|macos)
    for p in "${ALL_PLATFORMS[@]}"; do
      [[ "$p" == darwin/* ]] && PLATFORMS+=("$p")
    done
    ;;
  windows)
    PLATFORMS=("windows/amd64")
    ;;
  */*)
    PLATFORMS=("$TARGET")
    ;;
  *)
    echo "ERROR: Unknown target '$TARGET'." >&2
    echo "Usage: bash build.sh [all|current|linux|darwin|windows|<os/arch>]" >&2
    exit 1
    ;;
esac

# Build for each platform
STEP=2
for platform in "${PLATFORMS[@]}"; do
    GOOS="${platform%/*}"
    GOARCH="${platform#*/}"

    DIST="$DIR/dist/${APP_NAME}-${GOOS}-${GOARCH}"
    echo "[${STEP}/3] Building for ${GOOS}/${GOARCH} → $DIST"

    mkdir -p "$DIST"

    binary="${APP_NAME}"
    [[ "$GOOS" == "windows" ]] && binary+=".exe"

    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
        -ldflags="-s -w -X main.appVersion=$VERSION" \
        -trimpath \
        -o "$DIST/$binary" \
        .

    # Bundle supporting files
    echo "[3/3] Packaging assets..."
    cp "$DIR/LICENSE" "$DIR/LEGAL.md" "$DIR/THIRD_PARTY_NOTICES.md" "$DIST/"
    cp -R "$DIR/third_party" "$DIST/"
    mkdir -p "$DIST/static/fonts"
    cp "$DIR/static/fonts/OFL.txt" "$DIST/static/fonts/"
    if [[ -f "$DIR/config.json.example" ]]; then
        cp "$DIR/config.json.example" "$DIST/config.json.example"
    else
        cp "$DIR/config.json" "$DIST/config.json.example"
    fi
    mkdir -p "$DIST/downloads"

    # Copy scripts (Linux targets get systemd + install scripts)
    if [[ "$GOOS" == "linux" ]]; then
        cp "$DIR/install.sh" "$DIST/" 2>/dev/null || true
        cp "$DIR/uninstall.sh" "$DIST/" 2>/dev/null || true
        cp "$DIR/run.sh" "$DIST/" 2>/dev/null || true
        cp "$DIR/leecher.service" "$DIST/" 2>/dev/null || true
        chmod +x "$DIST"/*.sh 2>/dev/null || true
    fi

    chmod +x "$DIST/$binary"

    # Strip macOS extended attributes so tarballs are clean on Linux
    if command -v xattr &>/dev/null; then
        xattr -cr "$DIST" 2>/dev/null || true
    fi

    SIZE=$(du -sh "$DIST/$binary" | cut -f1)
    echo ""
    echo "  ✓ ${APP_NAME}-${GOOS}-${GOARCH} built (${SIZE})"
    echo ""

    STEP=$((STEP + 1))
done

echo "  Done. Run 'bash release.sh' to create tarballs."
echo ""
