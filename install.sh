#!/usr/bin/env bash
# install.sh — install spettro on macOS or Linux
#
# Usage:
#   curl -sSfL https://raw.githubusercontent.com/cesp99/spettro/main/install.sh | sh
#
# Or pin to a specific version:
#   VERSION=v1.2.3 curl -sSfL ... | sh

set -euo pipefail

REPO="cesp99/spettro"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# ── detect OS ───────────────────────────────────────────────────────────────
OS="$(uname -s)"
case "$OS" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux"  ;;
  *)
    echo "error: unsupported OS: $OS" >&2
    echo "spettro supports macOS and Linux." >&2
    exit 1
    ;;
esac

# ── detect architecture ──────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)          ARCH="amd64" ;;
  arm64|aarch64)   ARCH="arm64" ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

# ── resolve version ──────────────────────────────────────────────────────────
if [ -z "${VERSION:-}" ]; then
  echo "Fetching latest release..."
  VERSION="$(curl -sSfL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' \
    | sed -E 's/.*"([^"]+)".*/\1/')"
fi

if [ -z "$VERSION" ]; then
  echo "error: could not determine latest release version." >&2
  echo "Set VERSION manually: VERSION=v1.0.0 sh install.sh" >&2
  exit 1
fi

echo "Installing spettro ${VERSION} (${OS}/${ARCH})..."

# ── download ─────────────────────────────────────────────────────────────────
BINARY_NAME="spettro_${VERSION}_${OS}_${ARCH}"
TARBALL="${BINARY_NAME}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading ${URL}..."
if ! curl -sSfL "$URL" -o "${TMP}/${TARBALL}"; then
  echo "error: download failed." >&2
  echo "Check that version ${VERSION} exists at:" >&2
  echo "  https://github.com/${REPO}/releases" >&2
  exit 1
fi

tar -xzf "${TMP}/${TARBALL}" -C "${TMP}"

# ── install ───────────────────────────────────────────────────────────────────
if [ ! -d "$INSTALL_DIR" ]; then
  echo "error: install directory does not exist: ${INSTALL_DIR}" >&2
  exit 1
fi

DEST="${INSTALL_DIR}/spettro"

if [ -w "$INSTALL_DIR" ]; then
  cp "${TMP}/${BINARY_NAME}" "$DEST"
  chmod +x "$DEST"
else
  echo "sudo required to write to ${INSTALL_DIR}..."
  sudo cp "${TMP}/${BINARY_NAME}" "$DEST"
  sudo chmod +x "$DEST"
fi

# ── verify ────────────────────────────────────────────────────────────────────
if command -v spettro >/dev/null 2>&1; then
  echo ""
  echo "spettro ${VERSION} installed successfully."
  echo "Run 'spettro' to get started."
else
  echo ""
  echo "spettro ${VERSION} installed to ${DEST}."
  echo ""
  echo "Make sure ${INSTALL_DIR} is in your PATH:"
  echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
fi
