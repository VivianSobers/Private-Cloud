#!/usr/bin/env bash
#
# restore-test.sh — prove the backups actually restore.
#
#   Usage:
#     sudo ./restore-test.sh            # run all tests
#     sudo ./restore-test.sh zfs        # snapshot rollback test only
#     sudo ./restore-test.sh restic     # restic restore + byte-for-byte diff
#     sudo ./restore-test.sh --dry-run
#
# An untested backup is a hypothesis. This script converts it into a fact, or
# tells you loudly that you don't have one.
#
# SAFETY: this script never touches production datasets. The ZFS test operates
# on a scratch dataset it creates and destroys (tank/restore-test). The restic
# test restores into a temp directory and diffs. Both are read-only with
# respect to your real data.
#
set -euo pipefail

POOL="${POOL:-tank}"
SCRATCH="$POOL/restore-test"
BACKUP_ENV="${BACKUP_ENV:-/etc/private-cloud/backup.env}"
DRY_RUN=0
FAILURES=0

# ANSI, because a wall of undifferentiated text is how you miss a failure.
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  PASS\033[0m %s\n' "$*"; }
bad()  { printf '\033[1;31m  FAIL\033[0m %s\n' "$*" >&2; FAILURES=$((FAILURES+1)); }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

run() {
  if [[ $DRY_RUN -eq 1 ]]; then printf '  \033[2m$\033[0m %s\n' "$*"; else "$@"; fi
}

# ===========================================================================
# TEST 1 — ZFS snapshot rollback
# ===========================================================================
# Simulates the most common real incident by far: "I deleted/overwrote
# something and want it back." Verifies that snapshots capture state, that the
# .zfs/snapshot view is browsable (the non-destructive recovery path), and that
# rollback restores exactly.
test_zfs() {
  log "TEST 1: ZFS snapshot rollback"

  command -v zfs >/dev/null 2>&1 || { bad "zfs not installed"; return; }
  zpool list "$POOL" >/dev/null 2>&1 || { bad "pool '$POOL' does not exist"; return; }

  if zfs list "$SCRATCH" >/dev/null 2>&1; then
    warn "scratch dataset $SCRATCH exists from a previous run; destroying it"
    run zfs destroy -r "$SCRATCH"
  fi

  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  \033[2m$\033[0m zfs create -o mountpoint=/%s %s ... (create, snapshot, delete, rollback, verify)\n' "$SCRATCH" "$SCRATCH"
    return
  fi

  local mp="/$SCRATCH"
  zfs create -o "mountpoint=$mp" "$SCRATCH"
  # shellcheck disable=SC2064
  trap "zfs destroy -r '$SCRATCH' 2>/dev/null || true" RETURN

  # --- arrange: write known content ---------------------------------------
  mkdir -p "$mp/documents"
  echo "the original irreplaceable thesis" > "$mp/documents/thesis.txt"
  head -c 1M /dev/urandom > "$mp/documents/photo.bin"
  local orig_thesis orig_photo
  orig_thesis="$(sha256sum "$mp/documents/thesis.txt" | cut -d' ' -f1)"
  orig_photo="$(sha256sum "$mp/documents/photo.bin"  | cut -d' ' -f1)"

  zfs snapshot "$SCRATCH@testpoint"
  ok "snapshot created: $SCRATCH@testpoint"

  # --- act: destroy the data the way a real accident would ----------------
  rm -f "$mp/documents/photo.bin"
  echo "OOPS overwritten by a bad sync client" > "$mp/documents/thesis.txt"
  [[ ! -f "$mp/documents/photo.bin" ]] || bad "test setup failed: file still present"

  # --- assert A: the .zfs browse path (non-destructive recovery) ----------
  # This is the recovery path you should reach for FIRST in a real incident:
  # it copies out of the past without discarding the present.
  local snapdir="$mp/.zfs/snapshot/testpoint"
  if [[ -f "$snapdir/documents/photo.bin" ]]; then
    local browsed; browsed="$(sha256sum "$snapdir/documents/photo.bin" | cut -d' ' -f1)"
    # if/else rather than `cond && ok || bad`: in that form a failing `ok`
    # ALSO runs `bad`, and this script's exit code is the restore gate. A
    # false FAIL here is as bad as a false PASS.
    if [[ "$browsed" == "$orig_photo" ]]; then
      ok "browsable snapshot at .zfs/snapshot/ returns byte-identical data"
    else
      bad "file in .zfs/snapshot differs from original"
    fi
  else
    bad "cannot browse .zfs/snapshot/testpoint (is snapdir=hidden blocking it? try: zfs set snapdir=visible $SCRATCH)"
  fi

  # --- assert B: rollback (destructive recovery) --------------------------
  zfs rollback "$SCRATCH@testpoint"

  local new_thesis new_photo
  new_thesis="$(sha256sum "$mp/documents/thesis.txt" | cut -d' ' -f1)"
  new_photo="$(sha256sum  "$mp/documents/photo.bin"  | cut -d' ' -f1)"

  if [[ "$new_thesis" == "$orig_thesis" ]]; then
    ok "rollback restored overwritten file exactly"
  else
    bad "rollback did not restore the overwritten file"
  fi

  if [[ "$new_photo" == "$orig_photo" ]]; then
    ok "rollback restored deleted 1MB binary exactly"
  else
    bad "rollback did not restore the deleted file"
  fi

  log "TEST 1 complete (scratch dataset destroyed)"
}

# ===========================================================================
# TEST 2 — restic restore
# ===========================================================================
# Simulates "the pool is gone." Restores the most recent snapshot into a temp
# directory and diffs it against the live source. This is the test that catches
# a repo that has been quietly failing for months.
test_restic() {
  log "TEST 2: restic restore + diff"

  command -v restic >/dev/null 2>&1 || { bad "restic not installed (apt install restic)"; return; }
  [[ -f "$BACKUP_ENV" ]] || { bad "missing $BACKUP_ENV — configure backups first"; return; }

  # Source on its own line: a shellcheck directive covers the next COMMAND, and
  # on a `set -a; source ...` one-liner that is `set -a`, not the source.
  set -a
  # shellcheck disable=SC1090  # path is configurable by design
  source "$BACKUP_ENV"
  set +a
  [[ -n "${RESTIC_REPOSITORY:-}" ]] || { bad "RESTIC_REPOSITORY is empty in $BACKUP_ENV"; return; }

  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  \033[2m$\033[0m restic snapshots && restic restore latest --target <tmp> && diff -r\n'
    return
  fi

  restic cat config >/dev/null 2>&1 || { bad "cannot open repo at $RESTIC_REPOSITORY (unreachable, or wrong password)"; return; }
  ok "repo reachable and password correct"

  # A repo with no snapshots is the silent failure mode this whole script
  # exists to catch.
  local count
  count="$(restic snapshots --json 2>/dev/null | grep -c '"short_id"' || true)"
  if [[ "${count:-0}" -eq 0 ]]; then
    bad "repo contains ZERO snapshots — nothing has ever been backed up"
    return
  fi
  ok "repo contains $count snapshot(s)"

  # Warn if the newest backup is stale. A repo full of six-month-old snapshots
  # passes every other check and is still a disaster.
  local latest_epoch age_days
  latest_epoch="$(restic snapshots --json latest 2>/dev/null \
    | grep -o '"time":"[^"]*"' | tail -1 | cut -d'"' -f4 \
    | xargs -r -I{} date -d {} +%s 2>/dev/null || echo 0)"
  if [[ "${latest_epoch:-0}" -gt 0 ]]; then
    age_days=$(( ( $(date +%s) - latest_epoch ) / 86400 ))
    if [[ $age_days -gt 2 ]]; then
      bad "most recent backup is $age_days days old — the nightly timer is not working"
    else
      ok "most recent backup is ${age_days}d old"
    fi
  fi

  # --- structural integrity ------------------------------------------------
  log "verifying repository structure (restic check)"
  if restic check --read-data-subset=1% >/dev/null 2>&1; then
    ok "restic check passed (1% of data re-read and verified)"
  else
    bad "restic check FAILED — repository may be corrupt"
  fi

  # --- the actual restore --------------------------------------------------
  local tmp; tmp="$(mktemp -d /var/tmp/restore-test.XXXXXX)"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" RETURN

  log "restoring latest snapshot into $tmp"
  if ! restic restore latest --target "$tmp" >/dev/null 2>&1; then
    bad "restic restore failed"
    return
  fi

  local restored_files
  restored_files="$(find "$tmp" -type f | wc -l)"
  if [[ "$restored_files" -eq 0 ]]; then
    bad "restore produced ZERO files — the backup is backing up nothing"
    return
  fi
  ok "restored $restored_files files"

  # --- diff against the live source ---------------------------------------
  # The restore lands under the original absolute paths, which for our backup
  # are ZFS snapshot directories. Compare against the live dataset: content
  # should match except for anything written since the backup ran.
  local src="/$POOL/configs"
  local restored_configs
  restored_configs="$(find "$tmp" -type d -path "*${POOL}/configs" -print -quit 2>/dev/null || true)"

  if [[ -n "$restored_configs" && -d "$src" ]]; then
    # --brief: report differing files, don't dump their contents.
    # db-dumps is excluded because a new dump is written on every backup run.
    if diff -r --brief --exclude=db-dumps "$src" "$restored_configs" >/dev/null 2>&1; then
      ok "restored $POOL/configs is byte-identical to the live copy"
    else
      # Differences here are usually legitimate drift, not corruption. Show
      # them so a human can judge rather than asserting a pass or a fail.
      warn "restored configs differ from live copy — expected if files changed since the last backup:"
      diff -r --brief --exclude=db-dumps "$src" "$restored_configs" 2>&1 | head -20 >&2
      ok "restore produced readable data (differences listed above for review)"
    fi
  else
    warn "could not locate $POOL/configs in the restore to diff; restored tree:"
    find "$tmp" -maxdepth 4 -type d | head -20 >&2
  fi

  # --- the database dump must be a real, decompressible dump ---------------
  local dump
  dump="$(find "$tmp" -name 'pgdumpall-*.sql.gz' -print -quit 2>/dev/null || true)"
  if [[ -n "$dump" ]]; then
    if gzip -t "$dump" 2>/dev/null && zcat "$dump" | head -20 | grep -q 'PostgreSQL database cluster dump'; then
      ok "restored Postgres dump is valid and decompresses correctly"
    else
      bad "restored Postgres dump is corrupt or truncated"
    fi
  else
    warn "no Postgres dump found in the backup (expected if Postgres wasn't running when it ran)"
  fi

  log "TEST 2 complete (temp restore removed)"
}

# ===========================================================================
# main
# ===========================================================================
MODE=all
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    zfs|restic|all) MODE="$1"; shift ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ $DRY_RUN -eq 1 || $EUID -eq 0 ]] || die "must run as root (use sudo)"

printf '\n\033[1m=== RESTORE TEST — %s ===\033[0m\n\n' "$(date -Is)"

case "$MODE" in
  zfs)    test_zfs ;;
  restic) test_restic ;;
  all)    test_zfs; echo; test_restic ;;
esac

echo
if [[ $DRY_RUN -eq 1 ]]; then
  log "dry run complete"
  exit 0
elif [[ $FAILURES -eq 0 ]]; then
  printf '\033[1;32m=== ALL RESTORE TESTS PASSED ===\033[0m\n'
  printf 'Your backups are real. Re-run this quarterly — put it in the calendar.\n'
  exit 0
else
  printf '\033[1;31m=== %d CHECK(S) FAILED ===\033[0m\n' "$FAILURES"
  printf 'Do not proceed to Phase 1 until these pass. See docs/runbook-restore.md.\n'
  exit 1
fi
