#!/usr/bin/env bash
# Build a Debian/Ubuntu apt repository from the .deb files a release already
# produced, and sign its metadata if a GPG key is available.
#
# The inputs are exactly the packages client/build-release.sh writes with nfpm —
# nothing is rebuilt here. A repository is a layout plus an index plus a
# signature over that index, and this script is only those three things:
#
#   pool/main/p/pcsync/                 the .deb files, byte for byte
#   dists/<suite>/main/binary-<arch>/   Packages, Packages.gz
#   dists/<suite>/Release               the index of indexes: sizes and hashes
#   dists/<suite>/InRelease             Release with an inline signature
#   dists/<suite>/Release.gpg           the same signature, detached
#
# That signature is the ONLY thing apt actually verifies. Per-package .deb
# signatures are not checked by apt at all (see gpg-sign-packages.sh), so an
# unsigned repository is an unauthenticated one no matter how the packages were
# built. When PC_GPG_KEY is unset the repository is still generated — usable with
# [trusted=yes] for testing, and honestly labelled as such — because failing a
# release over a missing optional secret helps nobody.
#
# Usage:
#   scripts/release/build-apt-repo.sh <dist-dir> <out-dir> [suite]
#   PC_GPG_KEY=releases@example.com scripts/release/build-apt-repo.sh dist/ apt/
set -euo pipefail

dist="${1:?usage: build-apt-repo.sh <dist-dir> <out-dir> [suite]}"
out="${2:?usage: build-apt-repo.sh <dist-dir> <out-dir> [suite]}"
suite="${3:-stable}"
component="main"
origin="Private Cloud"

for tool in dpkg-scanpackages apt-ftparchive gzip; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "build-apt-repo: $tool is required (apt-get install dpkg-dev apt-utils)" >&2
    exit 1
  fi
done

debs=()
while IFS= read -r -d '' deb; do debs+=("$deb"); done \
  < <(find "$dist" -maxdepth 1 -name '*.deb' -print0 | sort -z)
if [ "${#debs[@]}" -eq 0 ]; then
  echo "build-apt-repo: no .deb files in $dist" >&2
  exit 1
fi

rm -rf "$out"
mkdir -p "$out/pool/$component/p/pcsync"
cp "${debs[@]}" "$out/pool/$component/p/pcsync/"

# Architectures are read off the packages rather than hardcoded, so adding an
# arch to build-release.sh does not silently produce a repository that omits it.
arches=()
while IFS= read -r arch; do
  [ -n "$arch" ] && arches+=("$arch")
done < <(for deb in "${debs[@]}"; do dpkg-deb --field "$deb" Architecture; done | sort -u)
echo "build-apt-repo: ${#debs[@]} package(s), architectures: ${arches[*]}"

for arch in "${arches[@]}"; do
  bindir="$out/dists/$suite/$component/binary-$arch"
  mkdir -p "$bindir"
  # Paths in Packages must be relative to the repository root, so the scan runs
  # from there — an absolute path here is a repository that only works on the
  # machine that built it.
  ( cd "$out" && dpkg-scanpackages --arch "$arch" "pool/$component" /dev/null ) \
    > "$bindir/Packages"
  gzip -9 -k -f "$bindir/Packages"
done

# apt-ftparchive writes the Release file: the hashes and sizes of every Packages
# index, which is what the signature below ends up covering transitively.
cat > "$out/apt-ftparchive.conf" <<CONF
APT::FTPArchive::Release::Origin "$origin";
APT::FTPArchive::Release::Label "pcsync";
APT::FTPArchive::Release::Suite "$suite";
APT::FTPArchive::Release::Codename "$suite";
APT::FTPArchive::Release::Components "$component";
APT::FTPArchive::Release::Architectures "${arches[*]}";
APT::FTPArchive::Release::Description "Private Cloud sync client";
CONF
apt-ftparchive -c "$out/apt-ftparchive.conf" release "$out/dists/$suite" \
  > "$out/dists/$suite/Release"
rm -f "$out/apt-ftparchive.conf"

if [ -z "${PC_GPG_KEY:-}" ]; then
  cat <<'NOTE'
build-apt-repo: PC_GPG_KEY is not set — the repository is UNSIGNED.
  apt will refuse it unless the source line says [trusted=yes], which turns the
  authentication check off. That is fine for a local test and wrong for anything
  a user installs from. Set PC_GPG_KEY to publish a real repository.
NOTE
else
  gpg_args=(--batch --yes --pinentry-mode loopback --local-user "$PC_GPG_KEY")
  if [ -n "${PC_GPG_PASSPHRASE:-}" ]; then
    gpg_args+=(--passphrase "$PC_GPG_PASSPHRASE")
  fi
  # InRelease (inline signature) is what modern apt fetches; Release.gpg is kept
  # for older clients, and both cover the same bytes.
  gpg "${gpg_args[@]}" --clearsign -o "$out/dists/$suite/InRelease" "$out/dists/$suite/Release"
  gpg "${gpg_args[@]}" --armor --detach-sign -o "$out/dists/$suite/Release.gpg" "$out/dists/$suite/Release"
  gpg --batch --yes --armor --export "$PC_GPG_KEY" > "$out/pcsync-archive-keyring.asc"
  echo "build-apt-repo: signed with $PC_GPG_KEY (InRelease, Release.gpg, public key exported)"
fi

cat > "$out/README.md" <<'DOC'
# pcsync apt repository

    curl -fsSL https://<host>/apt/pcsync-archive-keyring.asc \
      | sudo gpg --dearmor -o /usr/share/keyrings/pcsync-archive-keyring.gpg
    echo "deb [signed-by=/usr/share/keyrings/pcsync-archive-keyring.gpg] https://<host>/apt stable main" \
      | sudo tee /etc/apt/sources.list.d/pcsync.list
    sudo apt update && sudo apt install pcsync

`signed-by` pins this repository to this one key, so it can never sign for any
other package on the system. A repository added without it is trusted for
everything apt installs, which is not a trade worth making for one client.
DOC

echo "build-apt-repo: wrote $out"
