#!/bin/bash
# release.sh — create release tarballs and optionally upload to GitHub.
#
# Run after build.sh:
#   bash build.sh all
#   bash release.sh              # create tarballs
#   bash release.sh upload v1.0  # create tarballs and upload to GitHub
#
# Outputs:
#   release/leecher-<os>-<arch>.tar.gz
#   release/leecher-windows-amd64.zip

set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
DIST_DIR="$DIR/dist"
RELEASE_DIR="$DIR/release"
APP_NAME="leecher"
RELEASE_REPO="k-one-e/leecher"

if [[ ! -d "$DIST_DIR" ]]; then
    echo "ERROR: No build found at $DIST_DIR" >&2
    echo "       Run 'bash build.sh all' first." >&2
    exit 1
fi

mkdir -p "$RELEASE_DIR"

echo ""
echo "  Packaging release tarballs..."
echo ""

TARBALLS=()
for arch_dir in "$DIST_DIR"/${APP_NAME}-*; do
    [[ -d "$arch_dir" ]] || continue
    name="$(basename "$arch_dir")"

    # Rename to "leecher" so the tarball extracts to leecher/ regardless of platform
    mv "$arch_dir" "$DIST_DIR/$APP_NAME"

    if [[ "$name" == *windows* ]]; then
        tarball="$RELEASE_DIR/${name}.zip"
        (cd "$DIST_DIR" && zip -qr "$tarball" "$APP_NAME")
    else
        tarball="$RELEASE_DIR/${name}.tar.gz"
        COPYFILE_DISABLE=1 tar --no-xattrs --no-mac-metadata -czf "$tarball" -C "$DIST_DIR" "$APP_NAME" 2>/dev/null \
            || tar -czf "$tarball" -C "$DIST_DIR" "$APP_NAME"
    fi

    mv "$DIST_DIR/$APP_NAME" "$arch_dir"

    size=$(du -sh "$tarball" | cut -f1)
    echo "  ✓ $tarball ($size)"
    TARBALLS+=("$tarball")
done

if [[ ${#TARBALLS[@]} -eq 0 ]]; then
    echo "ERROR: No architectures found in $DIST_DIR" >&2
    exit 1
fi

echo ""

# Optional: upload to GitHub
if [[ "${1:-}" == "upload" ]]; then
    VERSION="${2:-}"
    if [[ -z "$VERSION" ]]; then
        echo "ERROR: Version required for upload." >&2
        echo "Usage: bash release.sh upload v1.0" >&2
        exit 1
    fi

    if ! command -v gh &>/dev/null; then
        echo "ERROR: GitHub CLI (gh) not found. Install it first." >&2
        exit 1
    fi

    echo "  Creating GitHub release $VERSION..."
    # Release notes come from RELEASE.md ({{VERSION}} is replaced).
    NOTES="$(sed "s|{{VERSION}}|${VERSION}|g" "$DIR/RELEASE.md")"

    if gh release view "$VERSION" --repo "$RELEASE_REPO" >/dev/null 2>&1; then
        echo "  Release $VERSION already exists on $RELEASE_REPO — updating assets and notes."
        gh release edit "$VERSION" --repo "$RELEASE_REPO" --title "$APP_NAME $VERSION" --notes "$NOTES"
        # --clobber replaces existing assets with the same filename.
        gh release upload "$VERSION" "${TARBALLS[@]}" --repo "$RELEASE_REPO" --clobber
    else
        gh release create "$VERSION" "${TARBALLS[@]}" \
            --repo "$RELEASE_REPO" \
            --title "$APP_NAME $VERSION" \
            --notes "$NOTES"
    fi

    echo ""
    echo "  ✓ Release $VERSION published."
    echo ""
else
    echo "  To upload to GitHub:"
    echo "    bash release.sh upload v1.0"
    echo ""
    echo "  Or manually:"
    echo "    gh release create v1.0 ${TARBALLS[*]} \\"
    echo "      --title 'leecher v1.0' --notes 'Pre-compiled binaries'"
    echo ""
fi
