#!/usr/bin/env bash
# Build a macOS installer package (.pkg) from the already-built pcsync binaries.
#
# WHAT IS AUTOMATED AND WHAT IS NOT — the honest version.
#
# Building the .pkg is fully automated and needs no secrets: pkgbuild ships with
# the Xcode command line tools and is on every GitHub macOS runner. The two
# darwin binaries are merged into one universal binary with lipo first, so a
# single package serves Apple Silicon and Intel.
#
# SIGNING is not automated, and cannot be, because it needs a paid Apple
# Developer ID — a "Developer ID Installer" certificate, which costs 99 USD a
# year and cannot be minted from CI or from a keyless flow. Notarization needs
# the same account. So:
#
#   * unset secrets  -> an UNSIGNED .pkg. Gatekeeper will refuse to open it by
#                       double-click; it installs from the command line with
#                       `sudo installer -pkg pcsync.pkg -target /`, and the
#                       cosign signature over SHA256SUMS still covers the file.
#   * secrets set    -> productsign runs, and notarytool submits and staples.
#
# This is the one place in the release where an absent secret genuinely reduces
# what a user gets, rather than merely skipping a nicety. Saying so is better
# than pretending the .pkg is signed.
#
# Usage:
#   scripts/release/build-macos-pkg.sh <dist-dir> <version> <out.pkg>
#
# Optional environment:
#   PC_MACOS_INSTALLER_IDENTITY  "Developer ID Installer: Name (TEAMID)"
#   PC_MACOS_NOTARY_PROFILE      a notarytool keychain profile name
set -euo pipefail

dist="${1:?usage: build-macos-pkg.sh <dist-dir> <version> <out.pkg>}"
version="${2:?missing version}"
out="${3:?missing output path}"
bare="${version#v}"

if ! command -v pkgbuild >/dev/null 2>&1; then
  echo "build-macos-pkg: pkgbuild not found — this script must run on macOS" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/root/usr/local/bin"

arm="$dist/pcsync-darwin-arm64"
amd="$dist/pcsync-darwin-amd64"
if [ -f "$arm" ] && [ -f "$amd" ] && command -v lipo >/dev/null 2>&1; then
  # One universal binary rather than two packages: the difference in download
  # size is a few megabytes, and "which one do I need" is a question no user
  # should have to answer about their own laptop.
  lipo -create -output "$work/root/usr/local/bin/pcsync" "$arm" "$amd"
  echo "build-macos-pkg: universal binary (arm64 + x86_64)"
elif [ -f "$arm" ]; then
  cp "$arm" "$work/root/usr/local/bin/pcsync"
  echo "build-macos-pkg: arm64 only"
elif [ -f "$amd" ]; then
  cp "$amd" "$work/root/usr/local/bin/pcsync"
  echo "build-macos-pkg: x86_64 only"
else
  echo "build-macos-pkg: no darwin binary in $dist" >&2
  exit 1
fi
chmod 0755 "$work/root/usr/local/bin/pcsync"

# The config template rides along as documentation. A real config carries a
# credential and has to be per-user, so the package never writes one.
mkdir -p "$work/root/usr/local/share/doc/pcsync"
if [ -f client/config.example.json ]; then
  cp client/config.example.json "$work/root/usr/local/share/doc/pcsync/"
fi

pkgbuild \
  --root "$work/root" \
  --identifier com.privatecloud.pcsync \
  --version "$bare" \
  --install-location / \
  "$work/unsigned.pkg"

if [ -z "${PC_MACOS_INSTALLER_IDENTITY:-}" ]; then
  cp "$work/unsigned.pkg" "$out"
  cat <<'NOTE'
build-macos-pkg: PC_MACOS_INSTALLER_IDENTITY is not set — the package is UNSIGNED.
  It installs with `sudo installer -pkg pcsync.pkg -target /`; Gatekeeper will
  block a double-click. Signing needs a paid Apple Developer ID Installer
  certificate, which no keyless flow can substitute for. docs/install.md says so
  to users too, rather than leaving them to discover it at the dialog box.
NOTE
  exit 0
fi

productsign --sign "$PC_MACOS_INSTALLER_IDENTITY" "$work/unsigned.pkg" "$out"
echo "build-macos-pkg: signed with $PC_MACOS_INSTALLER_IDENTITY"

if [ -n "${PC_MACOS_NOTARY_PROFILE:-}" ]; then
  xcrun notarytool submit "$out" --keychain-profile "$PC_MACOS_NOTARY_PROFILE" --wait
  xcrun stapler staple "$out"
  echo "build-macos-pkg: notarized and stapled"
else
  echo "build-macos-pkg: PC_MACOS_NOTARY_PROFILE unset — signed but NOT notarized;"
  echo "  first launch on a machine that has never seen this certificate may warn."
fi
