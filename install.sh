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
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

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
ARCHIVE_NAME="spettro_${VERSION}_${OS}_${ARCH}"
TARBALL="${ARCHIVE_NAME}.tar.gz"
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

# ── verify checksum ──────────────────────────────────────────────────────────
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
echo "Downloading checksums..."
if ! curl -sSfL "$CHECKSUMS_URL" -o "${TMP}/checksums.txt"; then
  echo "error: could not download checksums.txt from release ${VERSION}." >&2
  echo "Refusing to install without integrity verification." >&2
  exit 1
fi

# Extract the expected hash for our tarball
EXPECTED_HASH="$(grep "[[:space:]]${TARBALL}$" "${TMP}/checksums.txt" | awk '{print $1}')"
if [ -z "$EXPECTED_HASH" ]; then
  echo "error: no checksum found for ${TARBALL} in checksums.txt." >&2
  echo "Refusing to install without integrity verification." >&2
  exit 1
fi

# Compute the actual hash (portable across macOS and Linux)
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL_HASH="$(sha256sum "${TMP}/${TARBALL}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL_HASH="$(shasum -a 256 "${TMP}/${TARBALL}" | awk '{print $1}')"
else
  echo "error: neither sha256sum nor shasum is available." >&2
  echo "Cannot verify integrity." >&2
  exit 1
fi

if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
  echo "error: checksum verification failed!" >&2
  echo "  expected: ${EXPECTED_HASH}" >&2
  echo "  actual:   ${ACTUAL_HASH}" >&2
  echo "The downloaded tarball may be corrupted or tampered with." >&2
  echo "Refusing to install." >&2
  exit 1
fi

echo "Checksum verified."

tar -xzf "${TMP}/${TARBALL}" -C "${TMP}"

# ── install ───────────────────────────────────────────────────────────────────
# INSTALL_DIR defaults to a directory the current user already owns, so
# installing (and later self-updating in place) never requires sudo. Set
# INSTALL_DIR to override, e.g. INSTALL_DIR=/usr/local/bin, in which case you
# are responsible for its permissions.
mkdir -p "$INSTALL_DIR"

DEST="${INSTALL_DIR}/spettro"

if [ ! -w "$INSTALL_DIR" ]; then
  echo "error: ${INSTALL_DIR} is not writable." >&2
  echo "Choose a directory you own, e.g.: INSTALL_DIR=\$HOME/.local/bin sh install.sh" >&2
  exit 1
fi

# Remove any existing binary first: on macOS, cp onto an existing inode
# leaves the kernel's cached code signature stale and the new binary is
# SIGKILLed ("Code Signature Invalid") at launch.
rm -f "$DEST"
cp "${TMP}/spettro" "$DEST"
chmod +x "$DEST"

# ── verify ────────────────────────────────────────────────────────────────────
echo ""
echo "spettro ${VERSION} installed to ${DEST}."

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*)
    echo "Run 'spettro' to get started."
    ;;
  *)
    echo ""
    echo "${INSTALL_DIR} is not on your PATH. Add it, e.g.:"
    echo "  echo 'export PATH=\"${INSTALL_DIR}:\$PATH\"' >> ~/.bashrc  # or ~/.zshrc"
    ;;
esac
