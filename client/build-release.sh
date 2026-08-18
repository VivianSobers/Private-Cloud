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

# Linux packages (.deb/.rpm) from the just-built binaries — a pure repackage of
# an artifact we already produced, so it needs no Go toolchain. Built UNSIGNED on
# purpose: a locally installed package needs no signature, only a repository does
# (see nfpm.yaml). Skipped cleanly when nfpm isn't installed, since the binaries
# above are the deliverable and packaging is an extra.
pkg_version="${VERSION#v}" # deb/rpm versions start with a digit; drop a leading v
if command -v nfpm >/dev/null 2>&1; then
  for arch in amd64 arm64; do
    echo "  packaging linux/$arch (.deb, .rpm)"
    # Fill the template's placeholders for this target. sed rather than nfpm env
    # expansion because nfpm does not expand env vars inside contents.src.
    cfg="$DIST/nfpm-$arch.yaml"
    sed -e "s|__PC_ARCH__|$arch|g" \
        -e "s|__PC_VERSION__|$pkg_version|g" \
        -e "s|__PC_BIN__|$DIST/pcsync-linux-$arch|g" \
        nfpm.yaml > "$cfg"
    nfpm package -f "$cfg" -p deb -t "$DIST/"
    nfpm package -f "$cfg" -p rpm -t "$DIST/"
    rm -f "$cfg"
  done
else
  echo "  note: nfpm not found — skipping .deb/.rpm (binaries above are complete)"
  echo "        install: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"
fi

# Checksums over every artifact — the client that already verifies every synced
# chunk should not ask users to trust an unverified download of itself. Listing
# every file except the sums itself checksums each artifact exactly once, whether
# or not nfpm ran, with no glob that could double-count a package.
( cd "$DIST" && find . -maxdepth 1 -type f ! -name SHA256SUMS -printf '%f\n' \
    | sort | xargs sha256sum > SHA256SUMS )

echo "done -> $DIST/"
ls -1 "$DIST"
