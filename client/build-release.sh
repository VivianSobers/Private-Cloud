#!/usr/bin/env bash
# Cross-compile the pcsync client for every desktop target and package each with
# its config example and systemd unit. The client is pure-Go (no CGO), so this is
# a clean static cross-build with no per-OS toolchain — the property that makes
# shipping a desktop client tractable at all.
#
#   ./build-release.sh            # build every target into dist/
#   VERSION=1.2.0 ./build-release.sh
set -euo pipefail

cd "$(dirname "$0")"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
DIST="dist"
LDFLAGS="-s -w -X main.version=${VERSION}"

# os/arch pairs covering the desktops a sync client ships to.
TARGETS=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
)

rm -rf "$DIST"
mkdir -p "$DIST"

echo "pcsync ${VERSION} — building ${#TARGETS[@]} targets"
for target in "${TARGETS[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"

  out="pcsync-${os}-${arch}${ext}"
  echo "  $os/$arch -> $DIST/$out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$out" ./cmd/pcsync
done

# Checksums, so a download can be verified — the client that already verifies
# every synced chunk should not ask users to trust an unverified binary of itself.
( cd "$DIST" && sha256sum pcsync-* > SHA256SUMS )

echo "done -> $DIST/"
ls -1 "$DIST"
