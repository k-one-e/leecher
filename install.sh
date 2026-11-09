#!/bin/bash
# install.sh — one-time setup for leecher on a Linux machine.
#
# Run as root:
#   sudo bash install.sh
#
# What it does:
#   1. Creates a 'leecher' system user
#   2. Installs ffmpeg if missing
#   3. Creates downloads directory
#   4. Installs systemd service (if available)
#
# Note: yt-dlp and Node.js are managed by the leecher binary itself.

set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$DIR/leecher"

# ---------------------------------------------------------------------------
# Must run as root
# ---------------------------------------------------------------------------
if [[ $EUID -ne 0 ]]; then
    echo "  ERROR: install.sh must be run as root."
    echo "  Usage: sudo bash install.sh"
    exit 1
fi

echo ""
echo "  leecher — initial setup"
echo ""

# ---------------------------------------------------------------------------
# 1. Create leecher system user
# ---------------------------------------------------------------------------
if ! id -u leecher &>/dev/null; then
    echo "  [setup] Creating 'leecher' system user..."
    useradd --system --no-create-home --shell /usr/sbin/nologin leecher
else
    echo "  [setup] User 'leecher' already exists."
fi

# ---------------------------------------------------------------------------
# 2. Install yt-dlp and ffmpeg if missing
# ---------------------------------------------------------------------------
install_pkg() {
    local pkg="$1"
    if command -v apt-get &>/dev/null; then
        apt-get install -y -qq "$pkg"
    elif command -v dnf &>/dev/null; then
        dnf install -y -q "$pkg"
    elif command -v yum &>/dev/null; then
        yum install -y -q "$pkg"
    elif command -v pacman &>/dev/null; then
        pacman -S --noconfirm --needed "$pkg"
    elif command -v zypper &>/dev/null; then
        zypper install -y -q "$pkg"
    else
        echo "  WARNING: No supported package manager. Install '$pkg' manually." >&2
        return 1
    fi
}

if ! command -v ffmpeg &>/dev/null; then
    echo "  [setup] Installing ffmpeg..."
    install_pkg ffmpeg || true
fi

# Always install yt-dlp into $DIR/bin/ (the managed location used by the leecher binary).
# This is architecture-aware so it works correctly on ARM64 (Raspberry Pi).
mkdir -p "$DIR/bin"
YTDLP_BIN="$DIR/bin/yt-dlp"
if [[ ! -f "$YTDLP_BIN" ]]; then
    echo "  [setup] Downloading yt-dlp into $DIR/bin/..."
    ARCH="$(uname -m)"
    case "$ARCH" in
        aarch64|arm64) YTDLP_ASSET="yt-dlp_linux_aarch64" ;;
        armv7l|armv6l) YTDLP_ASSET="yt-dlp_linux_armv7l" ;;
        x86_64)        YTDLP_ASSET="yt-dlp_linux" ;;
        *)             YTDLP_ASSET="yt-dlp_linux" ;;
    esac
    if curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/latest/download/${YTDLP_ASSET}" \
            -o "$YTDLP_BIN"; then
        EXPECTED_SUM="$(curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/latest/download/SHA2-256SUMS" \
            | awk -v f="$YTDLP_ASSET" '$2 == f {print $1}')"
        ACTUAL_SUM="$(sha256sum "$YTDLP_BIN" 2>/dev/null | awk '{print $1}' \
            || shasum -a 256 "$YTDLP_BIN" | awk '{print $1}')"
        if [[ -z "$EXPECTED_SUM" || "$EXPECTED_SUM" != "$ACTUAL_SUM" ]]; then
            rm -f "$YTDLP_BIN"
            echo "  ERROR: yt-dlp checksum verification failed — refusing to install." >&2
            exit 1
        fi
        chmod +x "$YTDLP_BIN"
        echo "  [setup] yt-dlp downloaded and verified: $YTDLP_BIN"
    else
        echo "  WARNING: Failed to download yt-dlp. The leecher binary will retry on startup." >&2
    fi
else
    echo "  [setup] yt-dlp already present: $YTDLP_BIN"
fi

echo "  [setup] yt-dlp: $YTDLP_BIN"
echo "  [setup] ffmpeg: $(command -v ffmpeg 2>/dev/null || echo 'not found')"

NODE_BIN="$DIR/bin/node"
if [[ ! -f "$NODE_BIN" ]]; then
    echo "  [setup] Downloading Node.js LTS into $DIR/bin/..."
    ARCH="$(uname -m)"
    case "$ARCH" in
        aarch64|arm64) NODE_ARCH="linux-arm64" ;;
        armv7l|armv6l) NODE_ARCH="linux-armv7l" ;;
        x86_64)        NODE_ARCH="linux-x64" ;;
        *)             NODE_ARCH="linux-x64" ;;
    esac
    NODE_VERSION=$(curl -fsSL "https://nodejs.org/dist/index.json" | \
        python3 -c "import json,sys; d=json.load(sys.stdin); print(next((v['version'] for v in d if v.get('lts')), 'v22.14.0'))" 2>/dev/null \
        || echo "v22.14.0")
    NODE_URL="https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-${NODE_ARCH}.tar.gz"
    NODE_TARBALL="node-${NODE_VERSION}-${NODE_ARCH}.tar.gz"
    TMP_TAR="$(mktemp /tmp/node-XXXXXX.tar.gz)"
    TMP_DIR="$(mktemp -d /tmp/node-extract-XXXXXX)"
    echo "  [setup] Node.js ${NODE_VERSION} for ${NODE_ARCH}..."
    if curl -fsSL "$NODE_URL" -o "$TMP_TAR"; then
        # Verify against the release manifest before unpacking and executing it.
        EXPECTED_SUM="$(curl -fsSL "https://nodejs.org/dist/${NODE_VERSION}/SHASUMS256.txt" \
            | awk -v f="$NODE_TARBALL" '$2 == f {print $1}')"
        ACTUAL_SUM="$(sha256sum "$TMP_TAR" 2>/dev/null | awk '{print $1}' \
            || shasum -a 256 "$TMP_TAR" | awk '{print $1}')"
        if [[ -z "$EXPECTED_SUM" || "$EXPECTED_SUM" != "$ACTUAL_SUM" ]]; then
            rm -rf "$TMP_DIR" "$TMP_TAR"
            echo "  ERROR: Node.js checksum verification failed — refusing to install." >&2
            exit 1
        fi
        tar -xzf "$TMP_TAR" -C "$TMP_DIR" --strip-components=2 "node-${NODE_VERSION}-${NODE_ARCH}/bin/node"
        mv "$TMP_DIR/node" "$NODE_BIN"
        rm -rf "$TMP_DIR" "$TMP_TAR"
        chmod +x "$NODE_BIN"
        echo "  [setup] Node.js ${NODE_VERSION} installed: $NODE_BIN"
    else
        rm -rf "$TMP_DIR" "$TMP_TAR"
        echo "  WARNING: Failed to download Node.js. Falling back to package manager." >&2
        install_pkg nodejs || echo "  WARNING: Could not install nodejs. YouTube JS extraction may be limited." >&2
    fi
else
    echo "  [setup] Node.js already present: $NODE_BIN"
fi
echo "  [setup] nodejs: $NODE_BIN"

# ---------------------------------------------------------------------------
# 3. Verify binary exists
# ---------------------------------------------------------------------------
if [[ ! -f "$BINARY" ]]; then
    echo "  ERROR: leecher binary not found at $BINARY" >&2
    exit 1
fi
chmod +x "$BINARY"

# ---------------------------------------------------------------------------
# 4. Create config and downloads directory
# ---------------------------------------------------------------------------
CONFIG="$DIR/config.json"
if [[ ! -f "$CONFIG" && -f "$DIR/config.json.example" ]]; then
    echo "  [setup] Creating config.json from example..."
    cp "$DIR/config.json.example" "$CONFIG"
fi

DOWNLOADS_DIR="$DIR/downloads"
mkdir -p "$DOWNLOADS_DIR"
mkdir -p "$DIR/.pyi-tmp"
mkdir -p "$DIR/.cache/ytdlp"
mkdir -p "$DIR/state"

# ---------------------------------------------------------------------------
# 4b. Write yt-dlp path into config.json
# ---------------------------------------------------------------------------
YTDLP_BIN="$DIR/bin/yt-dlp"
if [[ -f "$YTDLP_BIN" && -f "$CONFIG" ]]; then
    sed -i "s|\"ytdlp_path\":.*|\"ytdlp_path\": \"$YTDLP_BIN\",|" "$CONFIG"
    echo "  [setup] Set ytdlp_path to $YTDLP_BIN in config.json"
fi

# ---------------------------------------------------------------------------
# 4c. Ownership — the install tree stays root-owned so a compromised yt-dlp or
#     ffmpeg (running as 'leecher') cannot rewrite the leecher binary. Only the
#     paths the service actually mutates are handed to the service user.
# ---------------------------------------------------------------------------
chown -R root:root "$DIR"
chown -R leecher:leecher "$DOWNLOADS_DIR" "$DIR/.cache" "$DIR/.pyi-tmp" "$DIR/bin" "$DIR/state"
chmod 700 "$DIR/state"
if [[ -f "$CONFIG" ]]; then
    # config.json holds the admin bcrypt hash and the cookies path.
    chown leecher:leecher "$CONFIG"
    chmod 600 "$CONFIG"
fi
chmod 755 "$BINARY"

echo ""
echo "  Setup complete. Start leecher with:"
echo "    bash $DIR/run.sh"
echo ""

# ---------------------------------------------------------------------------
# 5. Install systemd service (optional)
# ---------------------------------------------------------------------------
if command -v systemctl &>/dev/null; then
    SERVICE_FILE="/etc/systemd/system/leecher.service"
    TEMPLATE="$DIR/leecher.service"

    if [[ -f "$TEMPLATE" ]]; then
        echo "  [setup] Installing systemd service..."
        sed "s|__INSTALL_DIR__|$DIR|g" "$TEMPLATE" > "$SERVICE_FILE"
        systemctl daemon-reload
        systemctl enable leecher.service
        echo "  [setup] Service installed and enabled."
        echo ""
        echo "  Manage leecher as a service:"
        echo "    sudo systemctl start leecher"
        echo "    sudo systemctl stop leecher"
        echo "    sudo systemctl status leecher"
        echo "    journalctl -u leecher -f"
        echo ""
    fi
fi
