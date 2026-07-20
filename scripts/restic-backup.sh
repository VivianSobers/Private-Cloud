#!/usr/bin/env bash
#
# restic-backup.sh — nightly encrypted offsite backup.
#
#   Usage:
#     ./restic-backup.sh init     # one-time: create the repo
#     ./restic-backup.sh backup   # what the systemd timer runs
#     ./restic-backup.sh check    # verify repo integrity (weekly)
#     ./restic-backup.sh snapshots
#
# Config comes from an env file (default /etc/private-cloud/backup.env, or
# $BACKUP_ENV). It must define RESTIC_REPOSITORY and RESTIC_PASSWORD_FILE.
# See deploy/secrets/.env.example for the shape and target options.
#
# WHY A ZFS SNAPSHOT AND NOT THE LIVE DATASET:
#   Backing up a live directory tree gives you a smear across time — files
#   change while restic walks them. We snapshot first (atomic, instant, free)
#   and back up the snapshot's .zfs/snapshot view, so every backup is a
#   consistent point in time.
#
set -euo pipefail

BACKUP_ENV="${BACKUP_ENV:-/etc/private-cloud/backup.env}"
POOL="${POOL:-tank}"
SNAP_TAG="restic-$(date +%Y%m%d%H%M%S)"
HOSTNAME_TAG="$(hostname -s)"

# node_exporter textfile collector directory. The backup writes freshness
# metrics here after every run so Prometheus can alert when backups go stale.
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
BACKUP_PROM="$TEXTFILE_DIR/privatecloud_backup.prom"
# die() records a failure metric only while a backup is actually running, so a
# config-time death or a `check` failure doesn't masquerade as a failed backup.
RECORD_FAILURE_METRIC=0

log()  { printf '[%s] %s\n' "$(date -Is)" "$*"; }
warn() { printf '[%s] [warn] %s\n' "$(date -Is)" "$*" >&2; }
die()  {
  printf '[%s] [fail] %s\n' "$(date -Is)" "$*" >&2
  notify_failure "$*"
  [[ $RECORD_FAILURE_METRIC -eq 1 ]] && write_backup_metric failure
  exit 1
}

# --------------------------------------------------------------------------
# config
# --------------------------------------------------------------------------
[[ -f "$BACKUP_ENV" ]] || { echo "missing $BACKUP_ENV — copy deploy/secrets/.env.example and fill it in" >&2; exit 1; }
# shellcheck disable=SC1090
set -a; source "$BACKUP_ENV"; set +a

: "${RESTIC_REPOSITORY:?RESTIC_REPOSITORY not set in $BACKUP_ENV}"
: "${RESTIC_PASSWORD_FILE:?RESTIC_PASSWORD_FILE not set in $BACKUP_ENV}"
[[ -r "$RESTIC_PASSWORD_FILE" ]] || die "cannot read RESTIC_PASSWORD_FILE=$RESTIC_PASSWORD_FILE"

NTFY_URL="${NTFY_URL:-}"          # e.g. http://<tailscale-ip>:8080/private-cloud-critical
NTFY_TOKEN="${NTFY_TOKEN:-}"

# Retention: generous, because restic dedups aggressively and metadata is tiny.
KEEP_DAILY="${KEEP_DAILY:-14}"
KEEP_WEEKLY="${KEEP_WEEKLY:-8}"
KEEP_MONTHLY="${KEEP_MONTHLY:-12}"

# --------------------------------------------------------------------------
# notification
# --------------------------------------------------------------------------
notify() {
  local title="$1" body="$2" prio="${3:-default}" tags="${4:-}"
  [[ -n "$NTFY_URL" ]] || return 0
  local args=(-fsS --max-time 15 -H "Title: $title" -H "Priority: $prio")
  [[ -n "$tags"       ]] && args+=(-H "Tags: $tags")
  [[ -n "$NTFY_TOKEN" ]] && args+=(-H "Authorization: Bearer $NTFY_TOKEN")
  curl "${args[@]}" -d "$body" "$NTFY_URL" >/dev/null 2>&1 || \
    warn "ntfy notification failed (backup result above still stands)"
}
notify_failure() { notify "Backup FAILED on $HOSTNAME_TAG" "$1" urgent "rotating_light"; }

# --------------------------------------------------------------------------
# freshness metric (node_exporter textfile collector)
# --------------------------------------------------------------------------
# Writes the last success/failure timestamps so Prometheus can alert when
# backups silently stop. The AGE is deliberately NOT written here — a value
# frozen at write time would always read ~0; Prometheus derives
# privatecloud_backup_age_seconds live via a recording rule (see alerts.yml).
write_backup_metric() {
  local result="$1" now; now="$(date +%s)"
  mkdir -p "$TEXTFILE_DIR" 2>/dev/null || { warn "cannot write $TEXTFILE_DIR; skipping freshness metric"; return 0; }

  # Preserve whichever timestamp this run isn't updating.
  local last_success=0 last_failure=0
  if [[ -r "$BACKUP_PROM" ]]; then
    last_success="$(sed -n 's/^privatecloud_backup_last_success_timestamp \([0-9][0-9]*\)$/\1/p' "$BACKUP_PROM" | tail -1)"
    last_failure="$(sed -n 's/^privatecloud_backup_last_failure_timestamp \([0-9][0-9]*\)$/\1/p' "$BACKUP_PROM" | tail -1)"
  fi
  [[ "$last_success" =~ ^[0-9]+$ ]] || last_success=0
  [[ "$last_failure" =~ ^[0-9]+$ ]] || last_failure=0

  local run_success=0
  if [[ "$result" == success ]]; then last_success=$now; run_success=1; else last_failure=$now; fi

  local tmp; tmp="$(mktemp "${BACKUP_PROM}.XXXXXX")" || { warn "mktemp failed; skipping freshness metric"; return 0; }
  {
    echo "# HELP privatecloud_backup_last_success_timestamp Unix time of the last successful restic backup."
    echo "# TYPE privatecloud_backup_last_success_timestamp gauge"
    echo "privatecloud_backup_last_success_timestamp $last_success"
    echo "# HELP privatecloud_backup_last_failure_timestamp Unix time of the last failed restic backup."
    echo "# TYPE privatecloud_backup_last_failure_timestamp gauge"
    echo "privatecloud_backup_last_failure_timestamp $last_failure"
    echo "# HELP privatecloud_backup_last_run_success 1 if the most recent backup run succeeded, 0 if it failed."
    echo "# TYPE privatecloud_backup_last_run_success gauge"
    echo "privatecloud_backup_last_run_success $run_success"
  } > "$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$BACKUP_PROM"
}

# --------------------------------------------------------------------------
# snapshot helpers
# --------------------------------------------------------------------------
SNAPS_TAKEN=()

cleanup_snapshots() {
  for s in "${SNAPS_TAKEN[@]:-}"; do
    [[ -n "$s" ]] || continue
    zfs destroy "$s" 2>/dev/null || warn "could not destroy temp snapshot $s"
  done
}
trap cleanup_snapshots EXIT

take_snapshot() {
  local ds="$1" snap="$1@$SNAP_TAG"
  zfs snapshot "$snap"
  SNAPS_TAKEN+=("$snap")
  # The hidden .zfs/snapshot directory exposes the frozen tree read-only.
  local mp; mp="$(zfs get -H -o value mountpoint "$ds")"
  echo "$mp/.zfs/snapshot/$SNAP_TAG"
}

# --------------------------------------------------------------------------
# commands
# --------------------------------------------------------------------------
cmd_init() {
  if restic cat config >/dev/null 2>&1; then
    log "repo already initialized at $RESTIC_REPOSITORY"
  else
    log "initializing repo at $RESTIC_REPOSITORY"
    restic init
  fi
  log "BACK UP THE RESTIC PASSWORD. An unrecoverable password means an"
  log "unrecoverable repo — restic has no master key and no reset."
}

cmd_backup() {
  local started; started=$(date +%s)
  RECORD_FAILURE_METRIC=1        # from here on, any die() records a failed backup
  log "backup starting -> $RESTIC_REPOSITORY"

  restic cat config >/dev/null 2>&1 || die "repo not initialized or unreachable; run: $0 init"

  # ---- Postgres: logical dump, not a file copy ----------------------------
  # A crash-consistent file snapshot of PGDATA usually recovers, but a logical
  # dump is version-portable and restorable onto a fresh machine without
  # matching binaries. Phase 1 replaces this with pgBackRest WAL archiving for
  # point-in-time recovery; until then, nightly full dumps are the honest
  # answer and their RPO is 24h.
  local dump_dir="/$POOL/configs/db-dumps"
  mkdir -p "$dump_dir"; chmod 0700 "$dump_dir"
  if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx 'privatecloud-postgres'; then
    log "dumping postgres"
    docker exec privatecloud-postgres pg_dumpall -U "${POSTGRES_USER:-postgres}" \
      | gzip -1 > "$dump_dir/pgdumpall-$(date +%Y%m%d).sql.gz" \
      || die "pg_dumpall failed"
    # Keep only the last 3 local dumps; restic keeps the history.
    ls -1t "$dump_dir"/pgdumpall-*.sql.gz 2>/dev/null | tail -n +4 | xargs -r rm -f
  else
    warn "postgres container not running — skipping DB dump (file data still backed up)"
  fi

  # ---- snapshot the datasets we back up ----------------------------------
  local paths=()
  paths+=("$(take_snapshot "$POOL/configs")")
  paths+=("$(take_snapshot "$POOL/postgres")")
  # tank/blobs is intentionally NOT here in Phase 0: it is empty until the app
  # exists, and 3TB of blobs needs a sized offsite target. Add it in Phase 2:
  #   paths+=("$(take_snapshot "$POOL/blobs")")

  log "backing up: ${paths[*]}"
  restic backup \
    --host "$HOSTNAME_TAG" \
    --tag phase0 \
    --exclude-caches \
    --one-file-system \
    "${paths[@]}" \
    || die "restic backup failed"

  # ---- retention ---------------------------------------------------------
  log "pruning old snapshots"
  restic forget \
    --host "$HOSTNAME_TAG" \
    --keep-daily "$KEEP_DAILY" \
    --keep-weekly "$KEEP_WEEKLY" \
    --keep-monthly "$KEEP_MONTHLY" \
    --prune \
    || warn "restic forget/prune failed — backup itself succeeded"

  local elapsed=$(( $(date +%s) - started ))
  local stats; stats="$(restic stats --mode raw-data 2>/dev/null | tail -3 | tr '\n' ' ')"
  log "backup OK in ${elapsed}s. $stats"
  write_backup_metric success
  RECORD_FAILURE_METRIC=0        # success recorded; nothing after this is a backup failure
  notify "Backup OK on $HOSTNAME_TAG" "Completed in ${elapsed}s. $stats" low "white_check_mark"
}

cmd_check() {
  # --read-data-subset actually re-reads and verifies a slice of the data
  # rather than only checking metadata. A repo that "checks" without reading
  # data can still be silently corrupt.
  log "checking repo integrity"
  restic check --read-data-subset=5% || die "restic check FAILED — repo may be corrupt"
  log "repo check OK"
  notify "Backup repo check OK on $HOSTNAME_TAG" "restic check --read-data-subset=5% passed" low "white_check_mark"
}

cmd_snapshots() { restic snapshots; }

case "${1:-backup}" in
  init)      cmd_init ;;
  backup)    cmd_backup ;;
  check)     cmd_check ;;
  snapshots) cmd_snapshots ;;
  *) echo "usage: $0 {init|backup|check|snapshots}" >&2; exit 1 ;;
esac
