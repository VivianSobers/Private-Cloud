#!/usr/bin/env bash
# Optionally GPG-sign the Linux packages in a release directory.
#
# Signing here is OPTIONAL and the script exits 0 when it is not configured. That
# is a deliberate release-engineering choice: a repository signing key is a
# long-lived secret that not every fork or contributor will have, and a release
# that fails because an optional secret is absent is a release process nobody can
# run. When PC_GPG_KEY is unset this prints why it skipped and gets out of the
# way — the binaries and their cosign signatures, which need no long-lived key at
# all, are already the primary chain of trust.
#
# What is signed, and what actually gets checked:
#
#   .rpm  rpmsign embeds a signature in the package header, and dnf checks it
#         against an imported key. This is a real, enforced check.
#   .deb  dpkg-sig writes a signature into the archive — and apt does not look
#         at it. Debian dropped per-package verification years ago: what apt
#         verifies is the repository's InRelease signature, which is made by
#         build-apt-repo.sh. The .deb is signed here anyway, because some
#         organisations check it out of band, but nobody should mistake it for
#         the control that matters.
#
# Usage:
#   PC_GPG_KEY=<key id or email> [PC_GPG_PASSPHRASE=...] \
#     scripts/release/gpg-sign-packages.sh <dist-dir>
set -euo pipefail

dist="${1:?usage: gpg-sign-packages.sh <dist-dir>}"

if [ -z "${PC_GPG_KEY:-}" ]; then
  echo "gpg-sign-packages: PC_GPG_KEY is not set — skipping package signing."
  echo "  This is not an error. Packages installed from a file need no signature;"
  echo "  repository metadata does, and that is signed only when a key exists."
  exit 0
fi

if ! command -v gpg >/dev/null 2>&1; then
  echo "gpg-sign-packages: PC_GPG_KEY is set but gpg is not installed" >&2
  exit 1
fi

# A passphrase, when supplied, is fed to gpg over a file descriptor rather than
# put on a command line where every process on the machine can read it.
gpg_args=(--batch --yes --pinentry-mode loopback)
if [ -n "${PC_GPG_PASSPHRASE:-}" ]; then
  gpg_args+=(--passphrase-fd 0)
fi

sign_rpm() {
  local pkg="$1"
  if ! command -v rpmsign >/dev/null 2>&1; then
    echo "  skip $(basename "$pkg"): rpmsign not installed (install rpm-sign / rpm)" >&2
    return 0
  fi
  # rpmsign drives gpg itself; %__gpg_sign_cmd is overridden so the batch and
  # loopback flags above apply, which is what makes this work unattended in CI.
  rpmsign --define "_gpg_name ${PC_GPG_KEY}" \
    --define "__gpg_sign_cmd %{__gpg} gpg --batch --yes --pinentry-mode loopback ${PC_GPG_PASSPHRASE:+--passphrase-fd 3} --no-armor --no-secmem-warning -u \"%{_gpg_name}\" -sbo %{__signature_filename} %{__plaintext_filename}" \
    --addsign "$pkg" 3<<<"${PC_GPG_PASSPHRASE:-}"
  echo "  signed $(basename "$pkg") (rpm header signature; dnf enforces this)"
}

sign_deb() {
  local pkg="$1"
  if ! command -v dpkg-sig >/dev/null 2>&1; then
    echo "  skip $(basename "$pkg"): dpkg-sig not installed — apt checks the"
    echo "       repository InRelease signature, not this one, so nothing is lost"
    return 0
  fi
  if [ -n "${PC_GPG_PASSPHRASE:-}" ]; then
    printf '%s' "$PC_GPG_PASSPHRASE" | dpkg-sig -k "$PC_GPG_KEY" --sign builder \
      -g "$(printf '%s ' "${gpg_args[@]}")" "$pkg"
  else
    dpkg-sig -k "$PC_GPG_KEY" --sign builder "$pkg"
  fi
  echo "  signed $(basename "$pkg") (advisory; apt does not check it)"
}

echo "gpg-sign-packages: signing with key ${PC_GPG_KEY}"
found=0
for pkg in "$dist"/*.rpm; do
  [ -e "$pkg" ] || continue
  found=1
  sign_rpm "$pkg"
done
for pkg in "$dist"/*.deb; do
  [ -e "$pkg" ] || continue
  found=1
  sign_deb "$pkg"
done

if [ "$found" -eq 0 ]; then
  echo "gpg-sign-packages: no .deb or .rpm found in $dist — nothing to sign"
fi
