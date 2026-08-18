#!/usr/bin/env bash
#
# pgbackrest.sh — point-in-time recovery for the metadata database.
#
#   sudo ./scripts/pgbackrest.sh setup     # one-time: create the stanza
#   sudo ./scripts/pgbackrest.sh full      # weekly full backup
#   sudo ./scripts/pgbackrest.sh diff      # daily differential
#   sudo ./scripts/pgbackrest.sh check     # is archiving actually working?
#   sudo ./scripts/pgbackrest.sh info      # what can we recover to?
#
# The nightly pg_dumpall in restic-backup.sh is version-portable and restores
# onto a bare machine, which is what you want after losing everything. Its RPO
# is 24 hours, so a wrong UPDATE at lunchtime costs a day of metadata: every
# rename, every share, every grant since midnight.
#
# This closes that gap. It does not replace the dump — the dump survives this
# repository being the thing that broke, and two mechanisms that fail differently
# is the entire point of having two.
#
# Recovery is in docs/runbook-restore.md §4c.
set -euo pipefail

CONTAINER="${PG_CONTAINER:-privatecloud-postgres}"
STANZA="${PGBACKREST_STANZA:-privatecloud}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
PROM_FILE="$TEXTFILE_DIR/privatecloud_pgbackrest.prom"

log()  { printf '[pgbackrest] %s\n' "$*"; }
warn() { printf '[pgbackrest] WARNING: %s\n' "$*" >&2; }
die()  { printf '[pgbackrest] ERROR: %s\n' "$*" >&2; exit 1; }

pgbr() { docker exec -u postgres "$CONTAINER" pgbackrest --stanza="$STANZA" "$@"; }

running() {
  docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"
}

write_metrics() {
  local kind="$1" success="$2" duration="$3" now
  now="$(date +%s)"
  mkdir -p "$TEXTFILE_DIR"

  # Preserve the other backup kind's timestamps: a differential run must not
  # erase the record of when the last full succeeded, or the alert that watches
  # for a missing full would clear itself every night.
  local prev=""
  [[ -f "$PROM_FILE" ]] && prev="$(grep -v "kind=\"$kind\"" "$PROM_FILE" | grep -v '^#' || true)"

  local tmp="$PROM_FILE.$$"
  {
    echo "# HELP privatecloud_pgbackrest_last_success_timestamp Unix time of the last successful pgBackRest backup, by kind."
    echo "# TYPE privatecloud_pgbackrest_last_success_timestamp gauge"
    echo "# HELP privatecloud_pgbackrest_last_run_success 1 if the most recent pgBackRest run of this kind succeeded."
    echo "# TYPE privatecloud_pgbackrest_last_run_success gauge"
    echo "# HELP privatecloud_pgbackrest_duration_seconds Duration of the last pgBackRest run, by kind."
    echo "# TYPE privatecloud_pgbackrest_duration_seconds gauge"
    [[ -n "$prev" ]] && echo "$prev"
    if [[ "$success" -eq 1 ]]; then
      echo "privatecloud_pgbackrest_last_success_timestamp{kind=\"$kind\"} $now"
    fi
    echo "privatecloud_pgbackrest_last_run_success{kind=\"$kind\"} $success"
    echo "privatecloud_pgbackrest_duration_seconds{kind=\"$kind\"} $duration"
  } > "$tmp"
  mv -f "$tmp" "$PROM_FILE"
  chmod 0644 "$PROM_FILE"
}

backup() {
  local kind="$1" started rc=0
  running || die "$CONTAINER is not running"
  log "starting $kind backup"
  started="$(date +%s)"
  pgbr --type="$kind" backup || rc=$?
  local duration=$(( $(date +%s) - started ))
  if [[ $rc -eq 0 ]]; then
    log "$kind backup completed in ${duration}s"
    write_metrics "$kind" 1 "$duration"
  else
    warn "$kind backup FAILED after ${duration}s"
    write_metrics "$kind" 0 "$duration"
  fi
  return $rc
}

case "${1:-}" in
  setup)
    running || die "$CONTAINER is not running"
    log "creating stanza $STANZA"
    pgbr stanza-create
    # check is not optional here. stanza-create succeeding proves the
    # repository is writable; only check proves archive_command is actually
    # pushing, which is the half that silently does not work if archive_mode
    # was set without a restart.
    log "verifying that WAL archiving works"
    pgbr check
    log "stanza ready — take a full backup now: $0 full"
    ;;

  full|diff|incr)
    backup "$1"
    ;;

  check)
    running || die "$CONTAINER is not running"
    # This is the command worth running after any change to the Postgres
    # config. archive_mode requires a RESTART, not a reload, and a server with
    # archive_command set but archive_mode off logs nothing and archives
    # nothing.
    pgbr check && log "archiving and the repository are both healthy"
    ;;

  info)
    running || die "$CONTAINER is not running"
    pgbr info
    ;;

  *)
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
