#!/bin/bash
# uninstall.sh — remove leecher systemd service and optionally data.
#
# Run as root:
#   sudo bash uninstall.sh

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "  ERROR: uninstall.sh must be run as root."
    echo "  Usage: sudo bash uninstall.sh"
    exit 1
fi

SERVICE_FILE="/etc/systemd/system/leecher.service"

echo ""
echo "  leecher — uninstall"
echo ""

# Stop and remove systemd service
if [[ -f "$SERVICE_FILE" ]]; then
    echo "  [uninstall] Stopping service..."
    systemctl stop leecher.service 2>/dev/null || true
    systemctl disable leecher.service 2>/dev/null || true
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
    echo "  [uninstall] Service removed."
else
    echo "  [uninstall] No systemd service found — skipping."
fi

# Remove leecher system user
if id -u leecher &>/dev/null; then
    echo "  [uninstall] Removing 'leecher' system user..."
    userdel leecher 2>/dev/null || true
    echo "  [uninstall] User removed."
fi

# Optionally remove downloads
DIR="$(cd "$(dirname "$0")" && pwd)"
DOWNLOADS_DIR="$DIR/downloads"

if [[ -d "$DOWNLOADS_DIR" ]]; then
    echo ""
    read -rp "  Delete downloads directory ($DOWNLOADS_DIR)? [y/N] " answer
    if [[ "$answer" =~ ^[Yy]$ ]]; then
        rm -rf "$DOWNLOADS_DIR"
        echo "  [uninstall] Downloads directory removed."
    else
        echo "  [uninstall] Downloads directory kept."
    fi
fi

echo ""
echo "  Done."
echo ""
