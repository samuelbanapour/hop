#!/usr/bin/env bash
# Installs the latest hop release for this machine.
#
#   curl -fsSL https://raw.githubusercontent.com/samuelbanapour/hopcli/master/scripts/install.sh | sh
#
# Downloads the right binary for your OS/arch from GitHub Releases, verifies
# it against the release's SHA256SUMS, and drops it into $HOP_ROOT/bin
# (default ~/.hop/bin). Nothing is written outside that directory.
set -eu

REPO="samuelbanapour/hopcli"
ROOT="${HOP_ROOT:-$HOME/.hop}"

detect_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux)  echo linux ;;
    *) echo "hop has no build for $(uname -s) yet" >&2; exit 1 ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    arm64|aarch64) echo arm64 ;;
    *) echo "hop has no build for $(uname -m) yet" >&2; exit 1 ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

echo "==> Finding the latest hop release"
RELEASE_JSON="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest")"
TAG="$(printf '%s\n' "$RELEASE_JSON" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
if [ -z "$TAG" ]; then
  echo "could not find the latest hop release — see https://github.com/$REPO/releases" >&2
  exit 1
fi

ASSET="hop-${TAG}-${OS}-${ARCH}.tar.gz"
BASE_URL="https://github.com/$REPO/releases/download/$TAG"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

echo "==> Downloading $ASSET ($TAG)"
curl -fsSL "$BASE_URL/$ASSET" -o "$WORKDIR/$ASSET"
curl -fsSL "$BASE_URL/SHA256SUMS" -o "$WORKDIR/SHA256SUMS"

echo "==> Verifying checksum"
EXPECTED="$(grep " $ASSET\$" "$WORKDIR/SHA256SUMS" | awk '{print $1}')"
if [ -z "$EXPECTED" ]; then
  echo "no checksum for $ASSET in SHA256SUMS — refusing to install an unverified binary" >&2
  exit 1
fi
ACTUAL="$(sha256_of "$WORKDIR/$ASSET")"
if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "checksum mismatch for $ASSET" >&2
  echo "  expected $EXPECTED" >&2
  echo "  got      $ACTUAL" >&2
  exit 1
fi

echo "==> Installing to $ROOT/bin"
tar -xzf "$WORKDIR/$ASSET" -C "$WORKDIR"
BIN="$(find "$WORKDIR" -type f -name hop ! -name '*.tar.gz' | head -n1)"
if [ -z "$BIN" ]; then
  echo "could not find a hop binary inside $ASSET" >&2
  exit 1
fi
mkdir -p "$ROOT/bin"
install -m 0755 "$BIN" "$ROOT/bin/hop"

cat <<'BANNER'

   __
  / /_  ___  ____
 / __ \/ _ \/ __ \
/ / / / (_) / /_/ /
/_/ /_/\___/ .___/
          /_/
BANNER
echo "hop $TAG installed to $ROOT/bin/hop"
echo
echo "Add it to your PATH:"
echo
echo "    echo 'eval \"\$($ROOT/bin/hop shellenv)\"' >> ~/.zshrc"
echo
echo "Then restart your shell, or run that eval line directly in this one."
echo "First real run: hop install ripgrep fd bat jq"
