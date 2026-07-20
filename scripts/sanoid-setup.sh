#!/usr/bin/env bash
#
# sanoid-setup.sh — install sanoid and activate the snapshot policy.
#
#   Usage:
#     sudo ./sanoid-setup.sh [--dry-run]
#
# Non-destructive: installs a package, copies a config, enables a timer.
# Safe to re-run; it will overwrite /etc/sanoid/sanoid.conf with the repo copy,
# which is the intent — the repo is the source of truth.
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC_CONF="$REPO_ROOT/deploy/sanoid/sanoid.conf"
DST_DIR=/etc/sanoid
DST_CONF="$DST_DIR/sanoid.conf"
DRY_RUN=0

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

run() {
  if [[ $DRY_RUN -eq 1 ]]; then printf '  \033[2m$\033[0m %s\n' "$*"; else log "$*"; "$@"; fi
}

[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1
[[ $DRY_RUN -eq 1 || $EUID -eq 0 ]] || die "must run as root (use sudo)"
[[ -f "$SRC_CONF" ]] || die "missing $SRC_CONF — run this from a checkout of the repo"

# --------------------------------------------------------------------------
# 1. install
# --------------------------------------------------------------------------
# sanoid is packaged in Ubuntu 24.04 universe; it pulls in syncoid too, which
# Phase 0 uses for local replication and later phases use for offsite ZFS send.
if command -v sanoid >/dev/null 2>&1; then
  log "sanoid already installed: $(sanoid --version 2>&1 | head -1)"
else
  run apt-get update
  run apt-get install -y sanoid
fi

# --------------------------------------------------------------------------
# 2. config
# --------------------------------------------------------------------------
run mkdir -p "$DST_DIR"

if [[ $DRY_RUN -eq 1 ]]; then
  printf '  \033[2m$\033[0m install -m 0644 %s %s\n' "$SRC_CONF" "$DST_CONF"
else
  # Keep a dated backup of any hand-edited config before the repo overwrites it.
  if [[ -f "$DST_CONF" ]] && ! cmp -s "$SRC_CONF" "$DST_CONF"; then
    backup="$DST_CONF.$(date +%Y%m%d%H%M%S).bak"
    warn "existing $DST_CONF differs from repo; backing up to $backup"
    cp -a "$DST_CONF" "$backup"
  fi
  install -m 0644 "$SRC_CONF" "$DST_CONF"
  log "installed $DST_CONF"
fi

# --------------------------------------------------------------------------
# 3. validate against reality
# --------------------------------------------------------------------------
# A config naming datasets that don't exist silently snapshots nothing, which
# is the worst failure mode available: it looks fine until you need a restore.
if [[ $DRY_RUN -eq 0 ]]; then
  missing=0
  while read -r ds; do
    if ! zfs list "$ds" >/dev/null 2>&1; then
      warn "sanoid.conf references dataset that does not exist: $ds"
      missing=1
    fi
  done < <(grep -oP '^\[\Ktank/[^\]]+' "$DST_CONF" || true)
  [[ $missing -eq 0 ]] || die "fix the pool/datasets first (scripts/zfs-setup.sh), then re-run"

  if zfs list tank/staging >/dev/null 2>&1 && grep -q '^\[tank/staging\]' "$DST_CONF"; then
    die "tank/staging must NOT be snapshotted — remove it from sanoid.conf"
  fi
fi

# --------------------------------------------------------------------------
# 4. timer
# --------------------------------------------------------------------------
# The Ubuntu package ships sanoid.timer (runs sanoid --cron every 15 min, which
# is what makes frequent_period=15 meaningful). We enable rather than write our
# own unit so package updates keep working.
if [[ $DRY_RUN -eq 1 ]]; then
  printf '  \033[2m$\033[0m systemctl enable --now sanoid.timer\n'
elif systemctl list-unit-files sanoid.timer >/dev/null 2>&1 && \
     systemctl list-unit-files | grep -q '^sanoid\.timer'; then
  run systemctl enable --now sanoid.timer
  log "sanoid.timer enabled"
else
  warn "sanoid.timer not found in this package build; installing a fallback cron entry"
  echo '*/15 * * * * root /usr/sbin/sanoid --cron' > /etc/cron.d/sanoid
  chmod 0644 /etc/cron.d/sanoid
fi

# --------------------------------------------------------------------------
# 5. first run + proof
# --------------------------------------------------------------------------
if [[ $DRY_RUN -eq 1 ]]; then
  log "dry run complete"
  exit 0
fi

log "taking an initial snapshot set"
sanoid --take-snapshots --verbose || warn "initial snapshot run reported problems"
sanoid --prune-snapshots --verbose || true

log "current snapshots:"
zfs list -t snapshot -o name,used,refer,creation -s creation | tail -20

cat <<'EOF'

Verify tomorrow that the ladder is filling in:
    zfs list -t snapshot | wc -l
    systemctl list-timers sanoid.timer

Snapshots are NOT backups: they live on the same disks. scripts/restic-backup.sh
is what survives losing the pool.
EOF
