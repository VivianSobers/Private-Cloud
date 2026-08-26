#!/usr/bin/env bash
# Build a dnf/yum repository from the .rpm files a release already produced, and
# sign its metadata if a GPG key is available.
#
# Same shape as the apt side, and the same honesty about what is enforced. dnf
# checks two independent things: the signature in each package's header
# (gpg-sign-packages.sh puts it there, gpgcheck=1 enforces it) and the signature
# over repomd.xml (this script makes it, repo_gpgcheck=1 enforces it). The
# generated .repo file below turns both on when a key was available and says
# plainly when it could not.
#
# Usage:
#   scripts/release/build-dnf-repo.sh <dist-dir> <out-dir> [base-url]
#   PC_GPG_KEY=releases@example.com scripts/release/build-dnf-repo.sh dist/ rpm/
set -euo pipefail

dist="${1:?usage: build-dnf-repo.sh <dist-dir> <out-dir> [base-url]}"
out="${2:?usage: build-dnf-repo.sh <dist-dir> <out-dir> [base-url]}"
baseurl="${3:-https://REPLACE-ME/rpm}"

# createrepo_c is the maintained C implementation; the older Python createrepo is
# accepted as a fallback so this runs on an older box without a special case in
# the workflow.
createrepo=""
for candidate in createrepo_c createrepo; do
  if command -v "$candidate" >/dev/null 2>&1; then
    createrepo="$candidate"
    break
  fi
done
if [ -z "$createrepo" ]; then
  echo "build-dnf-repo: createrepo_c is required (apt-get install createrepo-c)" >&2
  exit 1
fi

rpms=()
while IFS= read -r -d '' rpm; do rpms+=("$rpm"); done \
  < <(find "$dist" -maxdepth 1 -name '*.rpm' -print0 | sort -z)
if [ "${#rpms[@]}" -eq 0 ]; then
  echo "build-dnf-repo: no .rpm files in $dist" >&2
  exit 1
fi

rm -rf "$out"
mkdir -p "$out"
cp "${rpms[@]}" "$out/"
echo "build-dnf-repo: ${#rpms[@]} package(s) -> $out"

"$createrepo" --quiet "$out"

gpgcheck=0
repo_gpgcheck=0
if [ -z "${PC_GPG_KEY:-}" ]; then
  cat <<'NOTE'
build-dnf-repo: PC_GPG_KEY is not set — repodata is UNSIGNED.
  The generated pcsync.repo therefore sets gpgcheck=0 and repo_gpgcheck=0, which
  is an unauthenticated repository. Usable for a local test, not for publishing.
NOTE
else
  gpg_args=(--batch --yes --pinentry-mode loopback --local-user "$PC_GPG_KEY")
  if [ -n "${PC_GPG_PASSPHRASE:-}" ]; then
    gpg_args+=(--passphrase "$PC_GPG_PASSPHRASE")
  fi
  # repomd.xml.asc is what repo_gpgcheck=1 verifies; it in turn carries the
  # checksums of every other metadata file, so one signature covers the index.
  gpg "${gpg_args[@]}" --armor --detach-sign -o "$out/repodata/repomd.xml.asc" "$out/repodata/repomd.xml"
  gpg --batch --yes --armor --export "$PC_GPG_KEY" > "$out/RPM-GPG-KEY-pcsync"
  gpgcheck=1
  repo_gpgcheck=1
  echo "build-dnf-repo: signed repomd.xml with $PC_GPG_KEY"
fi

cat > "$out/pcsync.repo" <<REPO
[pcsync]
name=Private Cloud sync client
baseurl=$baseurl
enabled=1
gpgcheck=$gpgcheck
repo_gpgcheck=$repo_gpgcheck
gpgkey=$baseurl/RPM-GPG-KEY-pcsync
REPO

cat > "$out/README.md" <<DOC
# pcsync dnf/yum repository

    sudo rpm --import $baseurl/RPM-GPG-KEY-pcsync
    sudo curl -fsSLo /etc/yum.repos.d/pcsync.repo $baseurl/pcsync.repo
    sudo dnf install pcsync

gpgcheck=$gpgcheck verifies each package's own signature; repo_gpgcheck=$repo_gpgcheck
verifies the signature over the repository index. Both are on only when this
repository was built with a signing key available.
DOC

echo "build-dnf-repo: wrote $out"
