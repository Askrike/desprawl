#!/bin/sh
# Installs desprawl from its GitHub releases, each of which carries this script:
#   curl -fsSL https://install.desprawl.org | sh
# (install.desprawl.org redirects to the latest release's install.sh.)
#
#   DESPRAWL_VERSION=v0.1.7   install that version instead of the latest
#   INSTALL_DIR=/usr/local/bin install there instead of ~/.local/bin
set -eu

REPO="https://github.com/askrike/desprawl"
VERSION="${DESPRAWL_VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
    Linux*) os=linux ;;
    Darwin*) os=darwin ;;
    MINGW* | MSYS* | CYGWIN*) os=windows ;;
    *) echo "desprawl: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) echo "desprawl: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# Names as in .goreleaser.yaml.
archive="desprawl_${os}_${arch}.tar.gz"
binary=desprawl
if [ "$os" = windows ]; then
    archive="desprawl_${os}_${arch}.zip"
    binary=desprawl.exe
fi

if [ "$VERSION" = latest ]; then
    # releases/latest redirects to the newest release's tag.
    VERSION="$(curl -fsSI -o /dev/null -w '%{redirect_url}' "$REPO/releases/latest" | sed -n 's|.*/releases/tag/||p')"
    if [ -z "$VERSION" ]; then
        echo "desprawl: no release found at $REPO/releases" >&2
        exit 1
    fi
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading desprawl $VERSION ($os/$arch)"
curl -fsSL "$REPO/releases/download/$VERSION/$archive" -o "$tmp/$archive"
curl -fsSL "$REPO/releases/download/$VERSION/checksums.txt" -o "$tmp/checksums.txt"

want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
else
    got="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "desprawl: checksum mismatch for $archive" >&2
    exit 1
fi

if [ "$os" = windows ]; then
    unzip -q "$tmp/$archive" -d "$tmp"
else
    tar xzf "$tmp/$archive" -C "$tmp"
fi
mkdir -p "$INSTALL_DIR"
# Rename into place, never copy over the old binary: macOS caches a binary's
# code signature by file, and kills it when run with new bytes in the old file.
cp "$tmp/$binary" "$INSTALL_DIR/.$binary.new"
chmod +x "$INSTALL_DIR/.$binary.new"
mv -f "$INSTALL_DIR/.$binary.new" "$INSTALL_DIR/$binary"
echo "Installed desprawl $VERSION to $INSTALL_DIR/$binary"

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        echo ""
        echo "$INSTALL_DIR is not on your PATH. Add this to your shell profile:"
        echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
        ;;
esac
echo ""
echo "Run 'desprawl setup' to add desprawl to your agents (Claude Code, Cursor, Codex, Gemini CLI)."
echo "Later, 'desprawl update' updates it."
