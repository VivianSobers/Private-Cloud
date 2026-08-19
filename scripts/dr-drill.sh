#!/usr/bin/env bash
#
# dr-drill.sh — rehearse the disaster-recovery runbook into a scratch location,
# on a schedule, and make the result a metric.
#
#   sudo ./scripts/dr-drill.sh            # run the rehearsal, export the result
#   sudo ./scripts/dr-drill.sh --status   # when did it last pass?
#   sudo ./scripts/dr-drill.sh --help
#
# restore-drill.sh answers "do the backups restore". This answers the question
# after it: "could this machine be rebuilt from them". They are not the same
# question, and the second one is answered today only by
# docs/runbook-disaster-recovery.md, which is a document. Documents rot in
# silence — the repository moves, a secret is renamed, a dataset stops being
# backed up, the git remote goes away — and every one of those is invisible
# until the day the server is gone.
#
# So this runs the parts of that runbook that can be run without a disaster:
# reach the offsite repository and read it, restore the datasets a rebuild
# cannot start without into a scratch directory, replay the database dump into a
# throwaway cluster, and confirm the deploy tree and the secrets the runbook
# demands are where it says they are. It times the restore, so the runbook's RTO
# is measured rather than asserted.
#
# WHAT THIS DOES NOT DO, stated here because a drill believed to prove more than
# it does is worse than no drill:
#   - It never restores over live data, and it is not a recovery tool.
#     Restoring into production stays a human decision taken with the runbook
#     open; the failure mode of automating it is overwriting good data with old
#     data, under time pressure, with no second chance.
#   - It does not rebuild a machine. Runbook phases A, B and D — install the OS,
#     create the pool, bring the stack up — remain manual, so the time measured
#     here is the data-restore leg only and not the whole RTO.
#   - It cannot see the printed passphrases in your safe. Those are the two
#     items with no recovery path at all, and no script can check them for you.
set -euo pipefail

REPO_DIR="${REPO_DIR:-/opt/private-cloud}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
PROM_FILE="$TEXTFILE_DIR/privatecloud_dr_drill.prom"
BACKUP_ENV="${BACKUP_ENV:-/etc/private-cloud/backup.env}"
LOG_DIR="${LOG_DIR:-/var/log/private-cloud}"
POOL="${POOL:-tank}"
PG_CONTAINER="${PG_CONTAINER:-privatecloud-postgres}"
PGBACKREST_STANZA="${PGBACKREST_STANZA:-privatecloud}"
COMPOSE_ENV="${COMPOSE_ENV:-$REPO_DIR/deploy/compose/.env}"

# Where the rehearsal writes. The default is deliberately outside every path
# that holds live data, and assert_scratch_is_scratch() enforces that whatever
# it is pointed at stays that way.
SCRATCH_ROOT="${DR_SCRATCH_ROOT:-/var/tmp/dr-drill}"

# Restoring file CONTENT needs as much scratch space as the pool holds, which is
# not something a monthly rehearsal can assume it has. Off by default: the
# metadata datasets restore in full, and blob data is covered by re-reading a
# sample of the repository instead. Turn it on where there is room, and the
# rehearsal then covers the whole restore.
DR_INCLUDE_BLOBS="${DR_INCLUDE_BLOBS:-false}"
# How much of the repository restic re-reads and re-checksums.
DR_READ_DATA_SUBSET="${DR_READ_DATA_SUBSET:-1%}"
# Refuse to restore into a filesystem with less room than this.
DR_MIN_FREE_KB="${DR_MIN_FREE_KB:-5242880}"
# A newest snapshot older than this means the DR position is not what the
# runbook claims. Generous next to the 24h RPO, because this is a monthly
# rehearsal and BackupTooOld already pages on the daily case.
DR_MAX_SNAPSHOT_AGE_DAYS="${DR_MAX_SNAPSHOT_AGE_DAYS:-3}"

# Named so that nobody, at any hour, mistakes it for the production container.
TMP_PG_CONTAINER="privatecloud-drdrill-postgres"

LOG_FILE=""
SCRATCH=""
STARTED=0
COMPLETED=0
FAILURES=0
RESTORE_SECONDS=0
RESTORE_MEASURED=0
RESTORED_BYTES=0

# Every stage the rehearsal knows about, in run order.
STAGES=(offsite restore postgres rebuild)
declare -A STAGE_RESULT=()

# --------------------------------------------------------------------------
# output
# --------------------------------------------------------------------------
# No ANSI here, unlike restore-test.sh: this runs from a timer, and its output
# goes to the journal, to a log file, and as a tail into a phone notification.
# Escape codes are noise in all three.
emit()     { printf '%s\n' "$*"; if [[ -n "$LOG_FILE" ]]; then printf '%s\n' "$*" >>"$LOG_FILE"; fi; }
emit_err() { printf '%s\n' "$*" >&2; if [[ -n "$LOG_FILE" ]]; then printf '%s\n' "$*" >>"$LOG_FILE"; fi; }

log()  { emit     "[dr-drill] $*"; }
ok()   { emit     "[dr-drill]   PASS $*"; }
note() { emit     "[dr-drill]   NOTE $*"; }
warn() { emit_err "[dr-drill] WARNING: $*"; }
bad()  { emit_err "[dr-drill]   FAIL $*"; FAILURES=$((FAILURES + 1)); }
die()  { emit_err "[dr-drill] ERROR: $*"; exit 1; }

usage() { sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'; }

# --------------------------------------------------------------------------
# metrics
# --------------------------------------------------------------------------
write_metrics() {
  local success="$1" duration="$2" now stage prev_success
  now="$(date +%s)"
  mkdir -p "$TEXTFILE_DIR" 2>/dev/null || { warn "cannot create $TEXTFILE_DIR; no metric written"; return 0; }

  # Carry the previous success timestamp forward on a failure. The age metric
  # has to keep counting from the last rehearsal that actually PASSED; resetting
  # it on a failed run would make a rotted recovery path look freshly verified,
  # which is exactly backwards.
  prev_success=0
  if [[ -f "$PROM_FILE" ]]; then
    prev_success="$(sed -n 's/^privatecloud_dr_drill_last_success_timestamp \([0-9][0-9]*\)$/\1/p' "$PROM_FILE" | tail -1)"
  fi
  [[ "$prev_success" =~ ^[0-9]+$ ]] || prev_success=0
  if [[ "$success" -eq 1 ]]; then prev_success="$now"; fi

  local tmp="$PROM_FILE.$$"
  {
    echo "# HELP privatecloud_dr_drill_last_success_timestamp Unix time of the last disaster-recovery rehearsal that passed."
    echo "# TYPE privatecloud_dr_drill_last_success_timestamp gauge"
    echo "privatecloud_dr_drill_last_success_timestamp $prev_success"
    echo "# HELP privatecloud_dr_drill_last_run_timestamp Unix time of the last disaster-recovery rehearsal, pass or fail."
    echo "# TYPE privatecloud_dr_drill_last_run_timestamp gauge"
    echo "privatecloud_dr_drill_last_run_timestamp $now"
    echo "# HELP privatecloud_dr_drill_last_run_success 1 if the most recent rehearsal passed."
    echo "# TYPE privatecloud_dr_drill_last_run_success gauge"
    echo "privatecloud_dr_drill_last_run_success $success"
    echo "# HELP privatecloud_dr_drill_duration_seconds How long the last rehearsal took end to end."
    echo "# TYPE privatecloud_dr_drill_duration_seconds gauge"
    echo "privatecloud_dr_drill_duration_seconds $duration"

    # Written only when a restore actually ran. A zero here on a run that died
    # before restoring would read as an instant recovery, which is the most
    # flattering possible lie about an RTO.
    if [[ "$RESTORE_MEASURED" -eq 1 ]]; then
      echo "# HELP privatecloud_dr_drill_restore_seconds Measured wall-clock seconds for the data-restore leg of the last rehearsal."
      echo "# TYPE privatecloud_dr_drill_restore_seconds gauge"
      echo "privatecloud_dr_drill_restore_seconds $RESTORE_SECONDS"
      echo "# HELP privatecloud_dr_drill_restored_bytes Bytes the last rehearsal restored into its scratch directory."
      echo "# TYPE privatecloud_dr_drill_restored_bytes gauge"
      echo "privatecloud_dr_drill_restored_bytes $RESTORED_BYTES"
    fi

    # Per stage, for diagnosis: which leg of the runbook broke is the first
    # question anybody asks. A stage that could not run is absent rather than 0,
    # so "we could not check this" never renders as "this is broken".
    echo "# HELP privatecloud_dr_drill_stage_success 1 if this stage of the rehearsal passed; absent if it could not run."
    echo "# TYPE privatecloud_dr_drill_stage_success gauge"
    for stage in "${STAGES[@]}"; do
      if [[ -n "${STAGE_RESULT["$stage"]:-}" ]]; then
        echo "privatecloud_dr_drill_stage_success{stage=\"$stage\"} ${STAGE_RESULT["$stage"]}"
      fi
    done
  } > "$tmp"
  # Atomic: node_exporter reads this directory continuously, and a half-written
  # file is a parse error that drops every metric in it.
  mv -f "$tmp" "$PROM_FILE"
  chmod 0644 "$PROM_FILE"
}

notify() {
  local title="$1" body="$2" prio="$3" topic="$4" tags="${5:-floppy_disk}"
  # shellcheck disable=SC1090  # path is configurable by design; nothing to follow
  if [[ -r "$BACKUP_ENV" ]]; then . "$BACKUP_ENV"; fi
  local url="${NTFY_URL:-http://127.0.0.1:8080}"
  local args=(-fsS --max-time 10 -H "Title: $title" -H "Priority: $prio" -H "Tags: $tags")
  if [[ -n "${NTFY_TOKEN:-}" ]]; then args+=(-H "Authorization: Bearer $NTFY_TOKEN"); fi
  curl "${args[@]}" -d "$body" "$url/$topic" >/dev/null 2>&1 ||
    warn "ntfy push failed (the rehearsal result above still stands)"
}

# --------------------------------------------------------------------------
# scratch safety
# --------------------------------------------------------------------------
# The runbook's real restore command is `restic restore latest --target /`. This
# script is the rehearsal of that command, and one edited variable is the whole
# distance between a rehearsal and an overwrite. So the target is checked against
# every path that holds something live before anything is written, and the check
# refuses rather than warns.
assert_scratch_is_scratch() {
  local root="$1" resolved live
  local live_paths=(/ /bin /boot /dev /etc /home /lib /lib64 /opt /proc /root /run
                    /sbin /srv /sys /usr /var/lib /var/log /var/spool
                    "/$POOL" "$REPO_DIR")

  [[ "$root" == /* ]] || die "DR_SCRATCH_ROOT must be an absolute path, got: $root"
  case "$root" in
    *..*) die "DR_SCRATCH_ROOT must not contain '..': $root" ;;
  esac
  # Resolve symlinks before judging. A scratch root that is a link into the pool
  # passes every textual test and still writes into live data.
  resolved="$(readlink -f -- "$root" 2>/dev/null || printf '%s' "$root")"
  [[ "$resolved" != "/" ]] || die "refusing to use / as the scratch root"

  for live in "${live_paths[@]}"; do
    [[ "$resolved" != "$live" ]] ||
      die "refusing to use $resolved as the scratch root: it is a live path"
    [[ "$resolved" != "$live"/* ]] ||
      die "refusing to use $resolved as the scratch root: it is inside the live path $live"
    [[ "$live" != "$resolved"/* ]] ||
      die "refusing to use $resolved as the scratch root: the live path $live is inside it"
  done
  SCRATCH_ROOT="$resolved"
}

cleanup_scratch() {
  if [[ -n "$SCRATCH" && -d "$SCRATCH" ]]; then
    rm -rf -- "$SCRATCH" || warn "could not remove $SCRATCH; it is holding disk space"
  fi
  docker rm -f "$TMP_PG_CONTAINER" >/dev/null 2>&1 || true
}

# Idempotence: a run killed by a systemd timeout or a reboot leaves its tree
# behind, and those trees are the size of a restore. Clear them before starting
# rather than accumulating copies of the backup on the scratch filesystem.
clean_stale_scratch() {
  local stale
  while IFS= read -r stale; do
    [[ -n "$stale" ]] || continue
    warn "removing a leftover scratch tree from an earlier run: $stale"
    rm -rf -- "$stale" || warn "could not remove $stale"
  done < <(find "$SCRATCH_ROOT" -mindepth 1 -maxdepth 1 -type d -name 'run.*' 2>/dev/null || true)
  docker rm -f "$TMP_PG_CONTAINER" >/dev/null 2>&1 || true
}

on_exit() {
  local rc=$?
  cleanup_scratch
  # An abort — a failing command under `set -e`, a systemd timeout, a signal — is
  # a rehearsal that did not pass, and it has to be recorded as one. Silence here
  # is how the age metric would keep looking healthy while nothing ever runs.
  if [[ "$COMPLETED" -eq 0 && "$STARTED" -ne 0 ]]; then
    warn "the rehearsal exited before finishing (exit $rc); recording it as a failure"
    write_metrics 0 "$(( $(date +%s) - STARTED ))"
    notify "DR DRILL FAILED" \
      "The rehearsal exited early with status $rc. Log: $LOG_FILE" \
      "urgent" "${NTFY_TOPIC_CRITICAL:-private-cloud-critical}" "rotating_light"
  fi
  exit "$rc"
}

# --------------------------------------------------------------------------
# STAGE 1 — the offsite repository
# --------------------------------------------------------------------------
# Runbook phase C begins with `restic snapshots`. If that cannot run from here it
# will not run from a rebuilt machine either, and the document stops at its third
# line.
stage_offsite() {
  log "--- stage offsite: can this machine reach and read the backup repository ---"

  if ! command -v restic >/dev/null 2>&1; then
    bad "restic is not installed; nothing about the offsite repository can be verified"
    return 0
  fi
  if [[ ! -r "$BACKUP_ENV" ]]; then
    bad "cannot read $BACKUP_ENV, which is where the repository location and password file are defined"
    return 0
  fi

  # Source on its own line: a shellcheck directive covers the NEXT COMMAND, and
  # on a `set -a; source ...` one-liner that is `set -a`, not the source.
  set -a
  # shellcheck disable=SC1090  # path is configurable by design; nothing to follow
  source "$BACKUP_ENV"
  set +a

  if [[ -z "${RESTIC_REPOSITORY:-}" ]]; then
    bad "RESTIC_REPOSITORY is empty in $BACKUP_ENV; the runbook cannot say where your backups are"
    return 0
  fi
  log "repository: $RESTIC_REPOSITORY"

  if ! restic cat config >>"$LOG_FILE" 2>&1; then
    bad "cannot open the repository at $RESTIC_REPOSITORY (unreachable, or the password is wrong)"
    return 0
  fi
  ok "repository is reachable and the password on this machine opens it"

  local snapshots count
  snapshots="$(restic snapshots --json 2>>"$LOG_FILE" || true)"
  count="$(grep -c '"short_id"' <<<"$snapshots" || true)"
  [[ "$count" =~ ^[0-9]+$ ]] || count=0
  if [[ "$count" -eq 0 ]]; then
    bad "the repository contains ZERO snapshots; there is nothing to rebuild from"
    return 0
  fi
  ok "repository holds $count snapshot(s)"

  local latest_iso latest_epoch age_days
  latest_iso="$(grep -o '"time":"[^"]*"' <<<"$snapshots" | tail -1 | cut -d'"' -f4 || true)"
  latest_epoch=0
  if [[ -n "$latest_iso" ]]; then
    latest_epoch="$(date -d "$latest_iso" +%s 2>/dev/null || echo 0)"
  fi
  if [[ "$latest_epoch" -gt 0 ]]; then
    age_days=$(( ( $(date +%s) - latest_epoch ) / 86400 ))
    if [[ "$age_days" -gt "$DR_MAX_SNAPSHOT_AGE_DAYS" ]]; then
      bad "the newest snapshot is ${age_days}d old; a rebuild today would bring back data from ${age_days} days ago"
    else
      ok "newest snapshot is ${age_days}d old"
    fi
  else
    warn "could not read the timestamp of the newest snapshot; its age was not checked"
  fi

  log "verifying repository structure and re-reading $DR_READ_DATA_SUBSET of the data"
  if restic check --read-data-subset="$DR_READ_DATA_SUBSET" >>"$LOG_FILE" 2>&1; then
    ok "restic check passed ($DR_READ_DATA_SUBSET of the data re-read and checksummed)"
  else
    bad "restic check FAILED; the repository may be corrupt. See $LOG_FILE"
  fi
  return 0
}

# --------------------------------------------------------------------------
# STAGE 2 — the restore itself, timed
# --------------------------------------------------------------------------
stage_restore() {
  log "--- stage restore: restore the newest snapshot into scratch and time it ---"

  if ! command -v restic >/dev/null 2>&1 || [[ -z "${RESTIC_REPOSITORY:-}" ]]; then
    bad "no usable restic configuration; the restore leg could not be rehearsed"
    return 0
  fi

  local avail
  avail="$(df -Pk "$SCRATCH" 2>/dev/null | tail -1 | awk '{print $4}' || true)"
  [[ "$avail" =~ ^[0-9]+$ ]] || avail=0
  if [[ "$avail" -lt "$DR_MIN_FREE_KB" ]]; then
    bad "only ${avail}K free on the scratch filesystem, under the ${DR_MIN_FREE_KB}K floor; point DR_SCRATCH_ROOT at a filesystem with room"
    return 0
  fi

  local target="$SCRATCH/restore"
  mkdir -p "$target"

  local args=(restore latest --target "$target")
  if [[ "$DR_INCLUDE_BLOBS" == "true" ]]; then
    log "restoring the whole snapshot, file content included"
  else
    # The datasets a rebuild cannot start without. File content is deliberately
    # left out by default; see DR_INCLUDE_BLOBS at the top of this file for what
    # that costs and what covers it instead.
    #
    # Both forms of each pattern on purpose: restic matches a path against a
    # pattern component by component, so the bare path selects the directory
    # node and only the `/**` form selects everything underneath it. With the
    # bare form alone this restores two empty directories and calls it a day.
    args+=(--include "/$POOL/configs"  --include "/$POOL/configs/**"
           --include "/$POOL/postgres" --include "/$POOL/postgres/**")
    log "restoring $POOL/configs and $POOL/postgres only (DR_INCLUDE_BLOBS=false)"
  fi

  local started
  started="$(date +%s)"
  if ! restic "${args[@]}" >>"$LOG_FILE" 2>&1; then
    bad "restic restore failed; runbook phase C does not work today. See $LOG_FILE"
    return 0
  fi
  RESTORE_SECONDS=$(( $(date +%s) - started ))
  RESTORE_MEASURED=1

  local restored_files
  restored_files="$(find "$target" -type f 2>/dev/null | wc -l || true)"
  [[ "$restored_files" =~ ^[0-9]+$ ]] || restored_files=0
  if [[ "$restored_files" -eq 0 ]]; then
    bad "the restore produced ZERO files; the backup holds nothing to rebuild from"
    return 0
  fi

  RESTORED_BYTES="$(du -sb "$target" 2>/dev/null | cut -f1 || true)"
  [[ "$RESTORED_BYTES" =~ ^[0-9]+$ ]] || RESTORED_BYTES=0
  ok "restored $restored_files files, $RESTORED_BYTES bytes, in ${RESTORE_SECONDS}s"

  # A restore that lands files but not the datasets the stack is built on is a
  # restore that fails at the moment it matters. Check them by name.
  local ds dir dir_files
  for ds in configs postgres; do
    dir="$(find "$target" -type d -path "*/$POOL/$ds" -print -quit 2>/dev/null || true)"
    if [[ -z "$dir" ]]; then
      bad "the restored tree contains no $POOL/$ds; that dataset is not in the backup"
      continue
    fi
    dir_files="$(find "$dir" -type f 2>/dev/null | wc -l || true)"
    [[ "$dir_files" =~ ^[0-9]+$ ]] || dir_files=0
    if [[ "$dir_files" -eq 0 ]]; then
      bad "$POOL/$ds restored as an empty tree; it is being backed up unmounted or excluded"
    else
      ok "$POOL/$ds restored with $dir_files file(s)"
    fi
  done

  # Throughput is the number that makes the runbook's phase C estimate real: it
  # is what lets you multiply by the size of the pool instead of guessing.
  if [[ "$RESTORE_SECONDS" -gt 0 && "$RESTORED_BYTES" -gt 0 ]]; then
    note "measured throughput: $(( RESTORED_BYTES / RESTORE_SECONDS / 1024 )) KiB/s, against this repository, from this machine"
  fi
  if [[ "$DR_INCLUDE_BLOBS" != "true" ]]; then
    note "this timing covers metadata only; a real phase C also moves file content, so treat it as a floor and not an estimate"
  fi
  return 0
}

# --------------------------------------------------------------------------
# STAGE 3 — the database
# --------------------------------------------------------------------------
stage_postgres() {
  log "--- stage postgres: can the database come back ---"

  local dump header
  dump="$(find "$SCRATCH" -name 'pgdumpall-*.sql.gz' -print 2>/dev/null | sort | tail -1 || true)"

  if [[ -z "$dump" ]]; then
    warn "no pg_dumpall dump in the restored snapshot; there is nothing to replay, so only the WAL archive is checked below"
  else
    header="$(zcat "$dump" 2>/dev/null | head -20 || true)"
    if gzip -t "$dump" 2>>"$LOG_FILE" && grep -q 'PostgreSQL database cluster dump' <<<"$header"; then
      ok "the restored dump is a complete gzip stream carrying a pg_dumpall header"
    else
      bad "the restored dump is corrupt or truncated: $dump"
    fi
    replay_dump "$dump"
  fi

  check_pgbackrest
  return 0
}

# Replay into a container with no volumes and no network. It cannot reach the
# production database and the production database cannot reach it, and --rm takes
# the anonymous data volume with it when it stops.
replay_dump() {
  local dump="$1" image waited=0 psql_log errors found tables db

  if ! command -v docker >/dev/null 2>&1; then
    warn "docker is not installed here, so the dump was not replayed; only its file integrity was checked"
    return 0
  fi

  image="${DR_PG_IMAGE:-}"
  if [[ -z "$image" ]]; then
    # Match the running cluster's image, so the replay is tested against the
    # version the data came from rather than whatever is newest.
    image="$(docker inspect --format '{{.Config.Image}}' "$PG_CONTAINER" 2>/dev/null || true)"
  fi
  if [[ -z "$image" ]]; then
    warn "cannot tell which Postgres image to use ($PG_CONTAINER is not present and DR_PG_IMAGE is unset); the dump was not replayed"
    return 0
  fi

  docker rm -f "$TMP_PG_CONTAINER" >/dev/null 2>&1 || true
  log "replaying the dump into a throwaway $image cluster"
  if ! docker run -d --rm --name "$TMP_PG_CONTAINER" --network none \
       -e POSTGRES_PASSWORD=drdrill-throwaway "$image" >>"$LOG_FILE" 2>&1; then
    bad "could not start a throwaway Postgres container from $image"
    return 0
  fi

  while [[ "$waited" -lt 90 ]]; do
    if docker exec "$TMP_PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then break; fi
    sleep 2
    waited=$((waited + 2))
  done
  if ! docker exec "$TMP_PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    bad "the throwaway cluster never became ready; the replay could not be attempted"
    docker rm -f "$TMP_PG_CONTAINER" >/dev/null 2>&1 || true
    return 0
  fi

  # ON_ERROR_STOP is deliberately off. pg_dumpall emits CREATE ROLE for roles a
  # fresh cluster already has, and stopping there would fail a replay that is in
  # fact fine. The assertion is made below on what the cluster CONTAINS
  # afterwards, which is the thing that actually matters.
  psql_log="$SCRATCH/psql-replay.log"
  if ! zcat "$dump" | docker exec -i "$TMP_PG_CONTAINER" psql -U postgres -q -f - >"$psql_log" 2>&1; then
    warn "psql exited non-zero during the replay; the two checks below decide whether that mattered"
  fi

  db="${POSTGRES_DB:-privatecloud}"
  found="$(docker exec "$TMP_PG_CONTAINER" psql -U postgres -tAc \
    "SELECT count(*) FROM pg_database WHERE datname = '$db'" 2>>"$LOG_FILE" | tr -d '[:space:]' || true)"
  if [[ "$found" == "1" ]]; then
    ok "the dump replayed into an empty cluster and recreated the '$db' database"
  else
    bad "after replaying the dump there is no '$db' database in the throwaway cluster"
  fi

  tables="$(docker exec "$TMP_PG_CONTAINER" psql -U postgres -d "$db" -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'" 2>>"$LOG_FILE" | tr -d '[:space:]' || true)"
  [[ "$tables" =~ ^[0-9]+$ ]] || tables=0
  if [[ "$tables" -gt 0 ]]; then
    ok "the replayed '$db' database has $tables table(s) in the public schema"
  else
    bad "the replayed '$db' database has no tables; the dump restored an empty shell"
  fi

  errors="$(grep -c '^ERROR:' "$psql_log" 2>/dev/null || true)"
  [[ "$errors" =~ ^[0-9]+$ ]] || errors=0
  if [[ "$errors" -gt 0 ]]; then
    note "psql logged $errors error line(s) during the replay; roles that already exist are expected, anything else is not. The first 20 are copied into $LOG_FILE"
    head -20 "$psql_log" >>"$LOG_FILE" 2>/dev/null || true
  fi

  docker rm -f "$TMP_PG_CONTAINER" >/dev/null 2>&1 || true
  return 0
}

check_pgbackrest() {
  local names
  if ! command -v docker >/dev/null 2>&1; then
    warn "docker is not installed here; the WAL archive was not checked"
    return 0
  fi
  names="$(docker ps --format '{{.Names}}' 2>/dev/null || true)"
  if ! grep -qx "$PG_CONTAINER" <<<"$names"; then
    warn "$PG_CONTAINER is not running, so the WAL archive was not checked. pgbackrest check has to ask a live cluster, and that is the one part of the database path a rehearsal cannot cover on a machine where Postgres is down"
    return 0
  fi
  if docker exec -u postgres "$PG_CONTAINER" pgbackrest --stanza="$PGBACKREST_STANZA" check >>"$LOG_FILE" 2>&1; then
    # Say exactly what this is worth. `check` pushes a WAL segment and reads it
    # back out of the repository, so it proves archiving works end to end and
    # that the repository is reachable. It restores nothing, and it is not
    # evidence that a point-in-time recovery would succeed.
    ok "pgbackrest check passed: WAL archiving works end to end and the repository is reachable. It performs no restore, so it is not proof that a point-in-time recovery would succeed"
  else
    bad "pgbackrest check FAILED; point-in-time recovery is not available. See $LOG_FILE"
  fi
  return 0
}

# --------------------------------------------------------------------------
# STAGE 4 — the things a rebuild needs that are not data
# --------------------------------------------------------------------------
# Phase D of the runbook is `git clone`, then secrets, then `docker compose up`.
# Every one of those has a way of being quietly untrue on a machine that has been
# running fine for a year.
stage_rebuild() {
  log "--- stage rebuild: deploy tree, secrets checklist, stack definition ---"

  local f
  for f in deploy/compose/docker-compose.yml scripts/restic-backup.sh scripts/restore-test.sh \
           scripts/zfs-setup.sh docs/runbook-disaster-recovery.md; do
    if [[ -e "$REPO_DIR/$f" ]]; then
      ok "deploy tree has $f"
    else
      bad "deploy tree is missing $f; the runbook refers to it by name"
    fi
  done

  local timers
  timers="$(find "$REPO_DIR/deploy/systemd" -maxdepth 1 -name '*.timer' 2>/dev/null | wc -l || true)"
  [[ "$timers" =~ ^[0-9]+$ ]] || timers=0
  if [[ "$timers" -gt 0 ]]; then
    ok "deploy tree has $timers systemd timer unit(s) for phase E"
  else
    bad "deploy tree has no systemd timers; a rebuilt machine would come back without backups"
  fi

  # The tree has to exist somewhere other than the machine the runbook assumes
  # you have lost. Phase D opens with `git clone`, and a clone needs a remote.
  if command -v git >/dev/null 2>&1 && git -C "$REPO_DIR" rev-parse --git-dir >/dev/null 2>&1; then
    local remote
    remote="$(git -C "$REPO_DIR" remote get-url origin 2>/dev/null || true)"
    if [[ -n "$remote" ]]; then
      ok "deploy tree is a git clone of $remote"
    else
      bad "deploy tree is a git repository with no origin remote; there is nowhere to clone it from once the machine is gone"
    fi
  else
    bad "$REPO_DIR is not a git clone; runbook phase D starts with 'git clone' and this configuration exists only on this machine"
  fi

  # Secrets. Existence and permissions only: this never reads or logs a value.
  check_secret_file "$BACKUP_ENV" "backup configuration"
  if [[ -n "${RESTIC_PASSWORD_FILE:-}" ]]; then
    check_secret_file "$RESTIC_PASSWORD_FILE" "restic repository password"
  else
    bad "RESTIC_PASSWORD_FILE is not set; without it the repository cannot be opened on a rebuilt machine"
  fi
  if [[ -e "$COMPOSE_ENV" ]]; then
    check_secret_file "$COMPOSE_ENV" "compose environment"
  else
    warn "no $COMPOSE_ENV on this machine; phase D recreates it from deploy/secrets/.env.example, so this is a gap only if you expected it to be here"
  fi

  # The pool passphrase. A script can see that the pool wants one and where it is
  # meant to come from. It cannot see whether the printed copy is in the safe, and
  # the printed copy is the one that matters after a fire.
  if command -v zfs >/dev/null 2>&1 && zfs list -H -o name "$POOL" >/dev/null 2>&1; then
    local keyloc
    keyloc="$(zfs get -H -o value keylocation "$POOL" 2>/dev/null || echo "-")"
    case "$keyloc" in
      prompt)   note "$POOL unlocks from a typed passphrase; this drill cannot verify that a printed copy exists, and nothing else can either" ;;
      file://*) check_secret_file "${keyloc#file://}" "ZFS pool key" ;;
      -|none)   note "$POOL reports no key location, which is expected if the pool is not encrypted" ;;
      *)        note "$POOL key location is $keyloc" ;;
    esac
  fi

  # The runbook's fill-in-the-blanks table. Underscores still in it mean nobody
  # has recorded where the backups live, which is discovered at the worst moment.
  local runbook="$REPO_DIR/docs/runbook-disaster-recovery.md"
  if [[ -r "$runbook" ]] && grep -q 'restic repository: *_\{4,\}' "$runbook"; then
    warn "the 'What you need before you start' table in runbook-disaster-recovery.md still has blanks; fill it in, or keep that information somewhere you can reach without this machine"
  fi

  # Phase D's last step. A compose file that does not parse is a rebuild that
  # stops after the clone.
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    local compose_args=(compose -f "$REPO_DIR/deploy/compose/docker-compose.yml")
    if [[ -e "$COMPOSE_ENV" ]]; then compose_args+=(--env-file "$COMPOSE_ENV"); fi
    compose_args+=(config)
    if docker "${compose_args[@]}" >/dev/null 2>>"$LOG_FILE"; then
      ok "the compose stack definition parses with the environment on this machine"
    else
      bad "docker compose config failed; phase D would stop at 'docker compose up'. See $LOG_FILE"
    fi
  else
    warn "docker compose is not available here; the stack definition was not validated"
  fi
  return 0
}

check_secret_file() {
  local path="$1" what="$2" mode
  if [[ ! -e "$path" ]]; then
    bad "$what is missing: $path"
    return 0
  fi
  if [[ ! -r "$path" ]]; then
    bad "$what exists but is not readable by this drill: $path"
    return 0
  fi
  mode="$(stat -c '%a' "$path" 2>/dev/null || echo "")"
  case "$mode" in
    600|400|640|440) ok "$what present at $path (mode $mode)" ;;
    "")              ok "$what present at $path" ;;
    *)               warn "$what at $path is mode $mode; a secret readable by more than root has a wider blast radius than it needs" ;;
  esac
  return 0
}

# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------
case "${1:-}" in
  --status)
    if [[ ! -f "$PROM_FILE" ]]; then
      echo "no DR drill has ever run (no $PROM_FILE)"
      exit 1
    fi
    last="$(sed -n 's/^privatecloud_dr_drill_last_success_timestamp \([0-9][0-9]*\)$/\1/p' "$PROM_FILE" | tail -1)"
    if [[ -z "$last" || "$last" == "0" ]]; then
      echo "the DR drill has run but has never passed"
      exit 1
    fi
    age=$(( ($(date +%s) - last) / 86400 ))
    echo "last successful DR drill: $(date -d "@$last" -Is) (${age} days ago)"
    restore_leg="$(sed -n 's/^privatecloud_dr_drill_restore_seconds \([0-9][0-9]*\)$/\1/p' "$PROM_FILE" | tail -1)"
    if [[ -n "$restore_leg" ]]; then
      echo "data-restore leg on the last run that restored: ${restore_leg}s"
    fi
    exit 0
    ;;
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) die "unknown argument: $1" ;;
esac

[[ $EUID -eq 0 ]] || die "must run as root (use sudo)"

assert_scratch_is_scratch "$SCRATCH_ROOT"
mkdir -p "$LOG_DIR" "$SCRATCH_ROOT"
chmod 0700 "$SCRATCH_ROOT"
stamp="$(date +%Y%m%dT%H%M%S)"
LOG_FILE="$LOG_DIR/dr-drill-$stamp.log"

STARTED="$(date +%s)"
trap on_exit EXIT

log "starting; scratch root $SCRATCH_ROOT, log $LOG_FILE"
clean_stale_scratch
SCRATCH="$(mktemp -d "$SCRATCH_ROOT/run.XXXXXX")"

# Dispatched by name rather than by building "stage_$stage" and calling it: an
# indirect call is one typo away from silently running nothing, and this way the
# set of stages that exist and the set that run cannot drift apart.
run_stage() {
  case "$1" in
    offsite)  stage_offsite ;;
    restore)  stage_restore ;;
    postgres) stage_postgres ;;
    rebuild)  stage_rebuild ;;
    *)        die "unknown stage: $1" ;;
  esac
}

for stage in "${STAGES[@]}"; do
  before="$FAILURES"
  run_stage "$stage"
  if [[ "$FAILURES" -eq "$before" ]]; then
    STAGE_RESULT["$stage"]=1
  else
    STAGE_RESULT["$stage"]=0
  fi
done

duration=$(( $(date +%s) - STARTED ))

# Keep the last dozen logs. A rehearsal that runs monthly for years should not
# quietly become the largest thing in /var/log.
# shellcheck disable=SC2012  # these names are generated above; no spaces, no newlines
ls -1t "$LOG_DIR"/dr-drill-*.log 2>/dev/null | tail -n +13 | xargs -r rm -f || true

summary="Rehearsed the recovery path into a scratch directory in ${duration}s."
if [[ "$RESTORE_MEASURED" -eq 1 ]]; then
  summary="$summary Data-restore leg: ${RESTORE_SECONDS}s for ${RESTORED_BYTES} bytes."
fi
summary="$summary It proves the backups can be read and replayed on this machine. It does not rebuild one: runbook phases A, B and D stay manual. Log: $LOG_FILE"

COMPLETED=1
if [[ "$FAILURES" -eq 0 ]]; then
  log "PASSED in ${duration}s"
  write_metrics 1 "$duration"
  notify "DR drill passed" "$summary" "low" "${NTFY_TOPIC:-private-cloud}" "white_check_mark"
  exit 0
fi

warn "FAILED: $FAILURES check(s) failed after ${duration}s"
write_metrics 0 "$duration"
notify "DR DRILL FAILED" \
  "$FAILURES check(s) failed in ${duration}s. The rebuild path is not what the runbook says it is."$'\n'"$(tail -20 "$LOG_FILE" 2>/dev/null)" \
  "urgent" "${NTFY_TOPIC_CRITICAL:-private-cloud-critical}" "rotating_light"
exit 1
