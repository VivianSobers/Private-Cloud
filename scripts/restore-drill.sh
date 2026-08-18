#!/usr/bin/env bash
#
# restore-drill.sh — run the restore test on a schedule and make the result a
# metric, so "we test our backups" is a claim Prometheus can check.
#
#   sudo ./scripts/restore-drill.sh          # run the drill, export the result
#   sudo ./scripts/restore-drill.sh --status # when did it last pass?
#
# restore-test.sh ends with "Re-run this quarterly — put it in the calendar." A
# calendar reminder is a person promising to remember, and the failure mode is
# silent: nothing anywhere goes red when the drill stops happening, so the first
# time you find out the restore path broke is the day you need it.
#
# This is that reminder as code. It runs the same script, writes
# privatecloud_restore_drill_* into the textfile collector the alerts already
# scrape, and pushes the outcome to ntfy. RestoreDrillTooOld then fires when the
# drill has not passed in a quarter — which is the alert that actually protects
# the backups, because it catches the drill going missing as well as failing.
set -uo pipefail

REPO_DIR="${REPO_DIR:-/opt/private-cloud}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
PROM_FILE="$TEXTFILE_DIR/privatecloud_restore_drill.prom"
BACKUP_ENV="${BACKUP_ENV:-/etc/private-cloud/backup.env}"
LOG_DIR="${LOG_DIR:-/var/log/private-cloud}"

log()  { printf '[restore-drill] %s\n' "$*"; }
warn() { printf '[restore-drill] WARNING: %s\n' "$*" >&2; }

# --------------------------------------------------------------------------
write_metrics() {
  local success="$1" duration="$2" now
  now="$(date +%s)"
  mkdir -p "$TEXTFILE_DIR"

  # Carry the previous success timestamp forward on a failure. The age metric
  # must keep counting from the last time the drill actually PASSED — resetting
  # it on a failed run would make a broken restore path look freshly verified,
  # which is precisely backwards.
  local last_success=0
  if [[ -f "$PROM_FILE" ]]; then
    last_success="$(sed -n 's/^privatecloud_restore_drill_last_success_timestamp \([0-9][0-9]*\)$/\1/p' "$PROM_FILE" | tail -1)"
    last_success="${last_success:-0}"
  fi
  [[ "$success" -eq 1 ]] && last_success="$now"

  local tmp="$PROM_FILE.$$"
  {
    echo "# HELP privatecloud_restore_drill_last_success_timestamp Unix time of the last restore drill that passed."
    echo "# TYPE privatecloud_restore_drill_last_success_timestamp gauge"
    echo "privatecloud_restore_drill_last_success_timestamp $last_success"
    echo "# HELP privatecloud_restore_drill_last_run_timestamp Unix time of the last restore drill, pass or fail."
    echo "# TYPE privatecloud_restore_drill_last_run_timestamp gauge"
    echo "privatecloud_restore_drill_last_run_timestamp $now"
    echo "# HELP privatecloud_restore_drill_last_run_success 1 if the most recent drill passed."
    echo "# TYPE privatecloud_restore_drill_last_run_success gauge"
    echo "privatecloud_restore_drill_last_run_success $success"
    echo "# HELP privatecloud_restore_drill_duration_seconds How long the last drill took."
    echo "# TYPE privatecloud_restore_drill_duration_seconds gauge"
    echo "privatecloud_restore_drill_duration_seconds $duration"
  } > "$tmp"
  # Atomic: node_exporter reads this directory continuously, and a half-written
  # file is a parse error that drops every metric in it.
  mv -f "$tmp" "$PROM_FILE"
  chmod 0644 "$PROM_FILE"
}

notify() {
  local title="$1" body="$2" prio="$3" topic="$4"
  # shellcheck disable=SC1090  # path is configurable by design; nothing to follow
  [[ -r "$BACKUP_ENV" ]] && . "$BACKUP_ENV"
  local url="${NTFY_URL:-http://127.0.0.1:8080}"
  curl -fsS --max-time 10 \
    ${NTFY_TOKEN:+-H "Authorization: Bearer $NTFY_TOKEN"} \
    -H "Title: $title" -H "Priority: $prio" -H "Tags: floppy_disk" \
    -d "$body" "$url/$topic" >/dev/null 2>&1 ||
    warn "ntfy push failed (the drill result above still stands)"
}

# --------------------------------------------------------------------------
if [[ "${1:-}" == "--status" ]]; then
  if [[ ! -f "$PROM_FILE" ]]; then
    echo "no drill has ever run (no $PROM_FILE)"
    exit 1
  fi
  last="$(sed -n 's/^privatecloud_restore_drill_last_success_timestamp \([0-9][0-9]*\)$/\1/p' "$PROM_FILE" | tail -1)"
  if [[ -z "$last" || "$last" == "0" ]]; then
    echo "the drill has run but has never passed"
    exit 1
  fi
  age=$(( ($(date +%s) - last) / 86400 ))
  echo "last successful restore drill: $(date -d "@$last" -Is) (${age} days ago)"
  exit 0
fi

[[ $EUID -eq 0 ]] || { warn "must run as root"; exit 1; }

mkdir -p "$LOG_DIR"
stamp="$(date +%Y%m%dT%H%M%S)"
logfile="$LOG_DIR/restore-drill-$stamp.log"

log "starting; output to $logfile"
started="$(date +%s)"

if "$REPO_DIR/scripts/restore-test.sh" all >"$logfile" 2>&1; then
  rc=0
else
  rc=$?
fi
duration=$(( $(date +%s) - started ))

# Keep the last dozen logs. A drill that runs quarterly for years should not
# quietly become the largest thing in /var/log.
ls -1t "$LOG_DIR"/restore-drill-*.log 2>/dev/null | tail -n +13 | xargs -r rm -f

if [[ $rc -eq 0 ]]; then
  log "PASSED in ${duration}s"
  write_metrics 1 "$duration"
  notify "Restore drill passed" \
    "Backups restored correctly in ${duration}s. Log: $logfile" \
    "low" "${NTFY_TOPIC:-private-cloud}"
  exit 0
fi

warn "FAILED (exit $rc) after ${duration}s"
write_metrics 0 "$duration"
notify "RESTORE DRILL FAILED" \
  "$(tail -20 "$logfile")" \
  "urgent" "${NTFY_TOPIC_CRITICAL:-private-cloud-critical}"
exit "$rc"
