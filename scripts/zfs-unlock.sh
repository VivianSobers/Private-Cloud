#!/usr/bin/env bash
#
# zfs-unlock.sh — load the pool's encryption key from a keyfile and mount.
#
# Driven by deploy/systemd/privatecloud-zfs-unlock.service, which is NOT enabled
# by default. Read the long comment at the top of that unit before enabling it:
# this script trades a security property for convenience, and the size of the
# trade depends entirely on where you put the keyfile.
#
#   PC_ZFS_KEYFILE   path to the raw key (no default, on purpose)
#   PC_ZFS_POOL      pool name (default: tank)
#
# Exit 75 (EX_TEMPFAIL) means "key not available, pool left locked". The unit
# treats that as success so a missing USB stick does not fail the boot: an
# unmounted pool is recoverable by walking to the machine, a failed boot is
# recoverable by walking to the machine with a keyboard.
set -uo pipefail

POOL="${PC_ZFS_POOL:-tank}"
KEYFILE="${PC_ZFS_KEYFILE:-}"

log()  { printf '[zfs-unlock] %s\n' "$*"; }
warn() { printf '[zfs-unlock] WARNING: %s\n' "$*" >&2; }

if [[ -z "$KEYFILE" ]]; then
  warn "PC_ZFS_KEYFILE is not set; leaving $POOL locked"
  warn "this unit does nothing until you choose where the key lives — see the unit file"
  exit 75
fi

if [[ ! -r "$KEYFILE" ]]; then
  warn "keyfile $KEYFILE is not readable; leaving $POOL locked"
  exit 75
fi

# Refuse a keyfile on the root filesystem. Storing the key beside the ciphertext
# it protects is not a weaker setup, it is no setup: anyone who takes the
# machine takes both halves. This check is here because it is exactly the
# shortcut somebody reaches for when the USB stick is missing and it is late.
key_src="$(findmnt -no SOURCE --target "$KEYFILE" 2>/dev/null || true)"
root_src="$(findmnt -no SOURCE --target / 2>/dev/null || true)"
if [[ -n "$key_src" && "$key_src" == "$root_src" ]]; then
  warn "refusing to use $KEYFILE: it is on the root filesystem"
  warn "the key would travel with the disks it protects, which makes the encryption decorative"
  exit 75
fi

perms="$(stat -c '%a' "$KEYFILE" 2>/dev/null || echo '')"
if [[ -n "$perms" && "$perms" != "400" && "$perms" != "600" ]]; then
  warn "keyfile $KEYFILE is mode $perms; 0400 is expected"
fi

state="$(zfs get -H -o value keystatus "$POOL" 2>/dev/null || echo unknown)"
if [[ "$state" == "available" ]]; then
  log "$POOL key already loaded"
else
  if ! zfs load-key -L "file://$KEYFILE" "$POOL"; then
    warn "zfs load-key failed for $POOL — wrong key, or the pool is not imported"
    exit 75
  fi
  log "loaded key for $POOL"
fi

# -l so any child dataset with its own key is picked up too; a partially mounted
# pool is the state where the API starts, finds an empty blob directory, and
# fsck reports every file as missing.
if ! zfs mount -a -l 2>/dev/null; then
  zfs mount -a || { warn "zfs mount -a failed"; exit 1; }
fi
log "mounted datasets under $POOL"
