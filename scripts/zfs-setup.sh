#!/usr/bin/env bash
#
# zfs-setup.sh — Phase 0 storage foundation.
#
# Creates an encrypted, compressed ZFS mirror named "tank" and the dataset
# layout the private-cloud stack expects.
#
#   Usage:
#     sudo ./zfs-setup.sh --dry-run /dev/disk/by-id/DISK1 /dev/disk/by-id/DISK2
#     sudo ./zfs-setup.sh           /dev/disk/by-id/DISK1 /dev/disk/by-id/DISK2
#
# THIS SCRIPT DESTROYS ALL DATA ON THE DISKS YOU PASS IT.
# Read it end to end, run it with --dry-run first, and confirm the device paths
# against `lsblk -o NAME,SIZE,MODEL,SERIAL` before running it for real.
#
# Use /dev/disk/by-id/ paths, NOT /dev/sdX. Kernel device letters are assigned
# in probe order and can swap between boots; by-id paths are stable and encode
# the serial number, so a pool built on them survives recabling.
#
set -euo pipefail

POOL="${POOL:-tank}"
ARC_MAX_BYTES="${ARC_MAX_BYTES:-6442450944}"   # 6 GiB — see "ARC sizing" below
DRY_RUN=0

# --------------------------------------------------------------------------
# plumbing
# --------------------------------------------------------------------------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

# run: echo the command in dry-run mode, execute it otherwise.
run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  \033[2m$\033[0m %s\n' "$*"
  else
    log "$*"
    "$@"
  fi
}

usage() {
  sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

# --------------------------------------------------------------------------
# argument parsing
# --------------------------------------------------------------------------
DISKS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage 0 ;;
    -*)        die "unknown flag: $1" ;;
    *)         DISKS+=("$1"); shift ;;
  esac
done

[[ ${#DISKS[@]} -eq 2 ]] || { warn "expected exactly 2 disk paths, got ${#DISKS[@]}"; usage 1; }
DISK1="${DISKS[0]}"
DISK2="${DISKS[1]}"

[[ $DRY_RUN -eq 1 ]] && log "DRY RUN — printing commands, changing nothing."

# --------------------------------------------------------------------------
# preflight
# --------------------------------------------------------------------------
[[ $DRY_RUN -eq 1 || $EUID -eq 0 ]] || die "must run as root (use sudo)"

if ! command -v zpool >/dev/null 2>&1; then
  log "ZFS not installed; installing zfsutils-linux"
  run apt-get update
  run apt-get install -y zfsutils-linux
fi

[[ "$DISK1" != "$DISK2" ]] || die "both disk arguments are the same device: $DISK1"

for d in "$DISK1" "$DISK2"; do
  if [[ ! -e "$d" ]]; then
    if [[ $DRY_RUN -eq 1 ]]; then
      warn "device does not exist (expected in dry-run on a different machine): $d"
    else
      die "device does not exist: $d"
    fi
  fi
  case "$d" in
    /dev/disk/by-id/*) : ;;
    *) warn "$d is not a /dev/disk/by-id path — unstable across reboots. Continuing, but reconsider." ;;
  esac
done

# Refuse to touch a disk that already carries a filesystem or partition table.
# This is the guard between "sets up my server" and "erases my life".
if [[ $DRY_RUN -eq 0 ]]; then
  for d in "$DISK1" "$DISK2"; do
    if blkid "$d" >/dev/null 2>&1; then
      warn "$d already contains a filesystem/partition signature:"
      blkid "$d" >&2 || true
      die "refusing to overwrite. Wipe it deliberately first: wipefs -a $d"
    fi
  done
fi

# Idempotency: never clobber an existing pool.
if zpool list "$POOL" >/dev/null 2>&1; then
  log "pool '$POOL' already exists — skipping pool creation, will ensure datasets"
  POOL_EXISTS=1
else
  POOL_EXISTS=0
fi

# --------------------------------------------------------------------------
# 1. pool creation
# --------------------------------------------------------------------------
# ashift=12  -> 4K sectors. Wrong at creation = permanently wrong; 12 is correct
#               for every modern 4TB drive and harmless on true-512b media.
# xattr=sa   -> store xattrs in the inode, big win for many-small-file metadata.
# acltype=posixacl -> needed if you ever expose datasets over Samba/NFS.
# atime=off  -> no write amplification on every read.
# encryption -> passphrase prompted interactively by ZFS itself; it is never
#               written to disk by this script, never passed as an argv, and
#               never logged. Store it in your password manager AND on paper.
if [[ $POOL_EXISTS -eq 0 ]]; then
  log "creating encrypted mirror pool '$POOL'"
  log "you will now be prompted for the pool encryption passphrase"
  run zpool create \
    -o ashift=12 \
    -o autotrim=on \
    -O encryption=aes-256-gcm \
    -O keyformat=passphrase \
    -O keylocation=prompt \
    -O compression=zstd \
    -O atime=off \
    -O xattr=sa \
    -O acltype=posixacl \
    -O dnodesize=auto \
    -O normalization=formD \
    -O canmount=off \
    -O mountpoint=none \
    "$POOL" mirror "$DISK1" "$DISK2"
fi

# --------------------------------------------------------------------------
# 2. datasets
# --------------------------------------------------------------------------
# Each dataset is tuned for its workload. recordsize is the single highest-
# impact knob: it caps the size of a logical block, so it should approximate
# the size of a typical write.
#
#   blobs    1M   — content-addressed chunks, written once, read sequentially.
#   postgres 16K  — matches PG's 8K page x 2; logbias=throughput avoids
#                   double-writing the WAL through the ZIL.
#   immich   1M   — photo/video originals, same profile as blobs.
#   staging  128K — upload scratch. Deliberately NOT snapshotted (see sanoid),
#                   because snapshotting half-written uploads only wastes space.
#   configs  128K — compose files, Caddyfile. Small, but snapshotted and backed
#                   up because it is the fastest path back to a working stack.
ensure_dataset() {
  local ds="$1"; shift
  if zfs list "$ds" >/dev/null 2>&1; then
    log "dataset $ds exists — applying properties"
    if [[ $DRY_RUN -eq 0 ]]; then
      for prop in "$@"; do run zfs set "$prop" "$ds"; done
    else
      for prop in "$@"; do printf '  \033[2m$\033[0m zfs set %s %s\n' "$prop" "$ds"; done
    fi
  else
    local args=()
    for prop in "$@"; do args+=(-o "$prop"); done
    run zfs create "${args[@]}" "$ds"
  fi
}

ensure_dataset "$POOL/blobs"    "recordsize=1M"   "mountpoint=/$POOL/blobs"
ensure_dataset "$POOL/postgres" "recordsize=16K"  "logbias=throughput" "mountpoint=/$POOL/postgres"
ensure_dataset "$POOL/immich"   "recordsize=1M"   "mountpoint=/$POOL/immich"
ensure_dataset "$POOL/configs"  "recordsize=128K" "mountpoint=/$POOL/configs"

# staging is marked with the conventional auto-snapshot property so that any
# snapshot tooling (sanoid, zfs-auto-snapshot) skips it, belt and braces
# alongside sanoid.conf simply not listing it.
ensure_dataset "$POOL/staging"  "recordsize=128K" "mountpoint=/$POOL/staging" "com.sun:auto-snapshot=false"

# --------------------------------------------------------------------------
# 3. ARC sizing
# --------------------------------------------------------------------------
# ZFS's ARC will otherwise grow toward half of RAM and fight Postgres for it.
# On a 16GB box: ~6GB ARC, leaving room for Postgres shared_buffers + page
# cache + the container stack. Tune later using `arc_summary` hit ratios.
ARC_CONF=/etc/modprobe.d/zfs.conf
if [[ $DRY_RUN -eq 1 ]]; then
  printf '  \033[2m$\033[0m echo "options zfs zfs_arc_max=%s" > %s\n' "$ARC_MAX_BYTES" "$ARC_CONF"
  printf '  \033[2m$\033[0m update-initramfs -u\n'
elif grep -qs "zfs_arc_max" "$ARC_CONF"; then
  log "zfs_arc_max already configured in $ARC_CONF — leaving it alone"
else
  log "capping ARC at $ARC_MAX_BYTES bytes in $ARC_CONF"
  echo "options zfs zfs_arc_max=${ARC_MAX_BYTES}" >> "$ARC_CONF"
  update-initramfs -u
  # Apply now as well so you don't have to reboot to get the cap.
  echo "$ARC_MAX_BYTES" > /sys/module/zfs/parameters/zfs_arc_max || \
    warn "could not set ARC live; it will apply after reboot"
fi

# --------------------------------------------------------------------------
# 4. unlock-at-boot + scrub schedule
# --------------------------------------------------------------------------
# An encrypted pool does NOT auto-mount after reboot: someone must supply the
# passphrase. That is the correct default for a machine that could be stolen.
# After every reboot, run:  zfs load-key -a && zfs mount -a
#
# Monthly scrub catches bit rot while the mirror still has a good copy to heal
# from. Ubuntu's zfsutils-linux ships /etc/cron.d/zfsutils-linux doing exactly
# this on the second Sunday; we verify rather than duplicate it.
if [[ $DRY_RUN -eq 0 ]]; then
  if [[ -f /etc/cron.d/zfsutils-linux ]]; then
    log "distro scrub cron present at /etc/cron.d/zfsutils-linux"
  else
    warn "no distro scrub cron found — add one:"
    warn "  echo '0 3 8-14 * 0 root zpool scrub $POOL' > /etc/cron.d/zfs-scrub"
  fi
fi

# --------------------------------------------------------------------------
# done
# --------------------------------------------------------------------------
if [[ $DRY_RUN -eq 1 ]]; then
  log "dry run complete — nothing was changed"
else
  log "pool and datasets ready"
  zpool status "$POOL"
  zfs list -o name,used,avail,recordsize,compression,encryption -r "$POOL"
  cat <<EOF

Next steps:
  1. Record the encryption passphrase in your password manager AND on paper.
     Losing it means losing every byte in the pool. There is no recovery.
  2. Verify it unlocks:  zfs unload-key -a && zfs load-key -a && zfs mount -a
  3. Run ./sanoid-setup.sh to enable snapshots.
EOF
fi
