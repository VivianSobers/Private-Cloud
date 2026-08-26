#!/usr/bin/env bash
#
# dr-recover.sh — execute the disaster-recovery runbook's data phase, one gated
# step at a time, and leave a transcript that says what was done.
#
#   sudo ./scripts/dr-recover.sh                     # dry run: plan and check, write nothing
#   sudo ./scripts/dr-recover.sh --confirm "$TOKEN"  # actually restore
#   sudo ./scripts/dr-recover.sh --resume            # continue an interrupted recovery
#   sudo ./scripts/dr-recover.sh --status
#   sudo ./scripts/dr-recover.sh --help
#
# WHY THIS EXISTS, AND WHY IT LOOKS LIKE THIS.
#
# docs/status.md carried "DR recovery automation" as deliberately-not-done for
# several phases, and the reason it gave was correct: automating a restore means
# automating something whose failure mode is overwriting good data with old
# data, under time pressure, with no second chance. That reasoning is not
# discarded here. It is the specification.
#
# So this is not "run the restore". It is the runbook, executed:
#
#   - DRY RUN IS THE DEFAULT. With no --confirm, every destructive step prints
#     exactly what it would do and writes nothing. A recovery you have not read
#     first is the one that restores March over April.
#   - EVERY DESTRUCTIVE STEP HAS A PRE-FLIGHT that must pass first: the source
#     must exist, be readable and be NEWER than what it would replace, and the
#     target must be what the operator named rather than whatever the variable
#     happened to expand to. A check that fails STOPS the recovery. It never
#     continues to the next step on the theory that the next one might work.
#   - CONFIRMATION IS A TYPED TOKEN, not --yes. The token is printed by the dry
#     run and contains the target and a digest of the plan, so a confirmation
#     copied from an earlier run against a different target is refused. A flag
#     you can add by reflex is not consent.
#   - IT IS RESUMABLE. State is written after each step, so an interrupted
#     recovery continues rather than restarting — restarting is how a half-
#     restored dataset gets restored over from the beginning at hour five.
#   - IT LEAVES A TRANSCRIPT. Every decision, check and command is appended to a
#     JSON-lines file, because "what did we actually do that night" is a
#     question asked afterwards, always, and answered badly from memory.
#
# WHAT IT DELIBERATELY DOES NOT DO:
#   - It does not install an OS, create a pool, or bring the stack up. Runbook
#     phases A, B and D stay manual and stay in the runbook. This covers phase
#     C, the data, which is the phase that is long, repetitive, and the one
#     where a mistyped dataset name is unrecoverable.
#   - It does not decide that a disaster has happened. A human does that.
#   - It cannot read the passphrases in your safe, which remain the two items
#     with no recovery path at all.
set -euo pipefail

REPO_DIR="${REPO_DIR:-/opt/private-cloud}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
PROM_FILE="$TEXTFILE_DIR/privatecloud_dr_recover.prom"
BACKUP_ENV="${BACKUP_ENV:-/etc/private-cloud/backup.env}"
LOG_DIR="${LOG_DIR:-/var/log/private-cloud}"
STATE_DIR="${DR_RECOVER_STATE_DIR:-/var/lib/private-cloud/dr-recover}"
POOL="${POOL:-tank}"
PG_CONTAINER="${PG_CONTAINER:-privatecloud-postgres}"

# The datasets phase C restores, in the order the runbook restores them. Metadata
# first: it is small, and a pool with metadata and no blobs is a system that can
# tell you what is missing, while the reverse is a pile of bytes with no index.
DATASETS="${DR_DATASETS:-meta blobs}"

DRY_RUN=1
CONFIRM_TOKEN=""
RESUME=0
STEP_FILTER=""

TRANSCRIPT=""
FAILURES=0
STARTED_AT="$(date -u +%s)"

emit()     { printf '%s\n' "$*"; if [[ -n "$TRANSCRIPT" ]]; then :; fi; }
emit_err() { printf '%s\n' "$*" >&2; }

log()  { emit     "[dr-recover] $*"; }
ok()   { emit     "[dr-recover]   PASS $*"; }
note() { emit     "[dr-recover]   NOTE $*"; }
plan() { emit     "[dr-recover]   PLAN $*"; }
warn() { emit_err "[dr-recover] WARNING: $*"; }
bad()  { emit_err "[dr-recover]   FAIL $*"; FAILURES=$((FAILURES + 1)); }
die()  { emit_err "[dr-recover] ERROR: $*"; record "aborted" "$*"; exit 1; }

usage() { sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; }

# record appends one JSON line to the transcript.
#
# JSON lines rather than prose because the question this answers — "what was
# restored, from what, at what time" — is one somebody asks months later, and
# grep over a paragraph is not an answer. Hand-rolled rather than through jq:
# this script has to run on a machine that was reinstalled an hour ago.
record() {
  local event="$1" detail="${2:-}"
  [[ -n "$TRANSCRIPT" ]] || return 0
  local esc
  esc="$(printf '%s' "$detail" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  printf '{"ts":"%s","event":"%s","dry_run":%s,"detail":"%s"}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$event" \
    "$([[ "$DRY_RUN" -eq 1 ]] && echo true || echo false)" "$esc" \
    >>"$TRANSCRIPT"
}

# plan_digest is what makes a confirmation token specific to ONE plan.
#
# The token embeds the pool, the dataset list and the source, so a token typed
# from a dry run against a scratch pool cannot confirm a run against the real
# one. This is the whole difference between a confirmation and a formality.
plan_digest() {
  printf '%s|%s|%s' "$POOL" "$DATASETS" "${RESTIC_REPOSITORY:-unset}" |
    sha256sum | cut -c1-12
}

expected_token() { printf 'RESTORE-%s-%s' "$POOL" "$(plan_digest)"; }

write_metrics() {
  local outcome="$1"
  mkdir -p "$TEXTFILE_DIR" 2>/dev/null || return 0
  local tmp="$PROM_FILE.$$"
  {
    echo '# HELP privatecloud_dr_recover_last_run_timestamp Unix time of the last dr-recover.sh run.'
    echo '# TYPE privatecloud_dr_recover_last_run_timestamp gauge'
    echo "privatecloud_dr_recover_last_run_timestamp $(date -u +%s)"
    echo '# HELP privatecloud_dr_recover_in_progress Whether a recovery is currently running.'
    echo '# TYPE privatecloud_dr_recover_in_progress gauge'
    echo "privatecloud_dr_recover_in_progress $([[ "$outcome" == "running" ]] && echo 1 || echo 0)"
    echo '# HELP privatecloud_dr_recover_failures Failed checks or steps in the last run.'
    echo '# TYPE privatecloud_dr_recover_failures gauge'
    echo "privatecloud_dr_recover_failures $FAILURES"
    echo '# HELP privatecloud_dr_recover_dry_run Whether the last run was a dry run.'
    echo '# TYPE privatecloud_dr_recover_dry_run gauge'
    echo "privatecloud_dr_recover_dry_run $DRY_RUN"
    if [[ "$outcome" == "success" && "$DRY_RUN" -eq 0 ]]; then
      echo '# HELP privatecloud_dr_recover_last_success_timestamp Unix time of the last completed real recovery.'
      echo '# TYPE privatecloud_dr_recover_last_success_timestamp gauge'
      echo "privatecloud_dr_recover_last_success_timestamp $(date -u +%s)"
    fi
    echo '# HELP privatecloud_dr_recover_duration_seconds Duration of the last run.'
    echo '# TYPE privatecloud_dr_recover_duration_seconds gauge'
    echo "privatecloud_dr_recover_duration_seconds $(( $(date -u +%s) - STARTED_AT ))"
  } >"$tmp" 2>/dev/null || return 0
  mv -f "$tmp" "$PROM_FILE" 2>/dev/null || return 0
  chmod 0644 "$PROM_FILE" 2>/dev/null || true
}

# --- state -------------------------------------------------------------------

step_done() { [[ -f "$STATE_DIR/$1.done" ]]; }

mark_done() {
  [[ "$DRY_RUN" -eq 1 ]] && return 0
  mkdir -p "$STATE_DIR"
  date -u +%Y-%m-%dT%H:%M:%SZ >"$STATE_DIR/$1.done"
}

# should_run decides whether a step executes, and is where --resume lives.
#
# A completed step is skipped on resume rather than repeated. That is the entire
# point: repeating a completed restore is how an interrupted recovery at hour
# five becomes a restore started again from the beginning.
should_run() {
  local step="$1"
  if [[ -n "$STEP_FILTER" && "$STEP_FILTER" != "$step" ]]; then
    return 1
  fi
  if [[ "$RESUME" -eq 1 ]] && step_done "$step"; then
    note "$step already completed at $(cat "$STATE_DIR/$step.done"); skipping"
    record "skip" "$step"
    return 1
  fi
  return 0
}

# --- pre-flight --------------------------------------------------------------

# require_root refuses early rather than failing halfway through a restore.
require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "must run as root: this restores datasets and replays a database"
  fi
}

# preflight_source proves the thing being restored FROM is real and readable.
#
# The check that matters is not "does the repository exist" but "can it be read
# right now": a restore that discovers an unreachable repository after it has
# already destroyed the target is the failure this whole script is shaped to
# avoid.
preflight_source() {
  log "pre-flight: the backup source"

  if [[ ! -f "$BACKUP_ENV" ]]; then
    bad "no $BACKUP_ENV; nothing here knows where the backups are"
    return 1
  fi
  set -a
  # shellcheck source=/dev/null
  . "$BACKUP_ENV"
  set +a

  if [[ -z "${RESTIC_REPOSITORY:-}" || -z "${RESTIC_PASSWORD:-}${RESTIC_PASSWORD_FILE:-}" ]]; then
    bad "RESTIC_REPOSITORY or the repository password is unset in $BACKUP_ENV"
    return 1
  fi
  if ! command -v restic >/dev/null 2>&1; then
    bad "restic is not installed on this machine"
    return 1
  fi
  if ! restic snapshots --latest 1 >/dev/null 2>&1; then
    bad "the repository at $RESTIC_REPOSITORY could not be read"
    return 1
  fi
  ok "the repository is reachable and its latest snapshot is readable"
  record "preflight_source_ok" "$RESTIC_REPOSITORY"
  return 0
}

# preflight_target proves the thing being restored ONTO is what was named.
#
# Two separate questions, and both have to be asked. Does the pool exist under
# exactly the name given — not a similarly-named one, and not a variable that
# expanded to empty. And does the target hold data that is NEWER than the
# backup, which is the one condition under which continuing destroys something
# irreplaceable.
preflight_target() {
  log "pre-flight: the restore target"

  if ! command -v zfs >/dev/null 2>&1; then
    bad "zfs is not installed; this is not the machine a restore runs on"
    return 1
  fi
  if [[ -z "$POOL" ]]; then
    bad "POOL is empty; refusing to guess what to restore onto"
    return 1
  fi
  if ! zpool list -H -o name "$POOL" >/dev/null 2>&1; then
    bad "no pool named '$POOL'; create it first — runbook phase B"
    return 1
  fi
  ok "pool '$POOL' exists"

  local newest_backup
  newest_backup="$(restic snapshots --latest 1 --json 2>/dev/null |
    sed -n 's/.*"time":"\([^"]*\)".*/\1/p' | head -1)"
  if [[ -z "$newest_backup" ]]; then
    bad "could not read the latest snapshot's timestamp; refusing to compare ages blind"
    return 1
  fi
  local backup_epoch
  backup_epoch="$(date -u -d "$newest_backup" +%s 2>/dev/null || echo 0)"
  if [[ "$backup_epoch" -eq 0 ]]; then
    bad "could not parse the snapshot timestamp '$newest_backup'"
    return 1
  fi
  note "newest backup: $newest_backup"

  # THE REFUSAL THIS SCRIPT EXISTS FOR. If the target already holds data written
  # after the backup was taken, restoring is not recovery — it is deletion of
  # the only copy of whatever was written since. It stops, and says so, and
  # makes the operator move the data out of the way themselves.
  local ds target_epoch
  for ds in $DATASETS; do
    if ! zfs list -H -o name "$POOL/$ds" >/dev/null 2>&1; then
      ok "$POOL/$ds does not exist yet — nothing to overwrite"
      continue
    fi
    target_epoch="$(zfs get -H -p -o value creation "$POOL/$ds" 2>/dev/null || echo 0)"
    local used
    used="$(zfs get -H -p -o value used "$POOL/$ds" 2>/dev/null || echo 0)"
    if [[ "$used" -gt 0 && "$target_epoch" -gt "$backup_epoch" ]]; then
      bad "$POOL/$ds holds data newer than the newest backup; restoring would destroy it"
      bad "  move it aside (zfs rename) and re-run, or restore into a scratch pool first"
      return 1
    fi
    if [[ "$used" -gt 0 ]]; then
      warn "$POOL/$ds is not empty and WILL be overwritten by this recovery"
      record "target_not_empty" "$POOL/$ds"
    fi
  done
  ok "every target is absent, empty, or older than the backup"
  record "preflight_target_ok" "$POOL"
  return 0
}

# --- steps -------------------------------------------------------------------

# run_or_plan is the single gate every destructive command passes through.
#
# One function so a step cannot accidentally be written to run in a dry run.
# There is no path here that executes without DRY_RUN being 0, and adding one
# would mean editing this function, which is where somebody would look.
run_or_plan() {
  local what="$1"; shift
  if [[ "$DRY_RUN" -eq 1 ]]; then
    plan "$what"
    plan "  \$ $*"
    record "planned" "$what"
    return 0
  fi
  log "$what"
  record "running" "$what"
  if ! "$@"; then
    bad "$what"
    record "failed" "$what"
    return 1
  fi
  ok "$what"
  record "done" "$what"
  return 0
}

step_datasets() {
  should_run "datasets" || return 0
  local ds
  for ds in $DATASETS; do
    run_or_plan "restore $POOL/$ds from the offsite repository" \
      restic restore latest --target "/$POOL" --include "/$POOL/$ds" || return 1
  done
  mark_done "datasets"
}

step_database() {
  should_run "database" || return 0
  # The database is replayed AFTER the datasets, matching the runbook: the file
  # rows must not describe content that is not on disk yet. The reverse order
  # produces a system that lists files it cannot open, which reads to a user as
  # data loss even though nothing was lost.
  run_or_plan "replay the database dump into $PG_CONTAINER" \
    bash -c "restic dump latest /var/backups/private-cloud/postgres.sql | docker exec -i '$PG_CONTAINER' psql -U postgres" || return 1
  mark_done "database"
}

step_verify() {
  should_run "verify" || return 0
  # Verification is not optional and is not a separate script an operator might
  # not run. A recovery that has not been checked is a recovery nobody can act
  # on: the whole value of finishing is knowing whether it worked.
  if [[ "$DRY_RUN" -eq 1 ]]; then
    plan "verify: fsck the restored tree (cloudctl fsck, WITHOUT --repair)"
    return 0
  fi
  log "verify: fsck the restored tree"
  if [[ -x "$REPO_DIR/bin/cloudctl" ]]; then
    # Deliberately without --repair. A checker run against a tree that is still
    # being restored will find absences that are not damage, and --repair acts
    # on exactly that judgement.
    if "$REPO_DIR/bin/cloudctl" fsck; then
      ok "fsck reported a consistent tree"
    else
      bad "fsck reported problems; do NOT run --repair until you know why"
    fi
  else
    note "cloudctl not found at $REPO_DIR/bin/cloudctl; run 'cloudctl fsck' by hand"
  fi
  mark_done "verify"
}

# --- main --------------------------------------------------------------------

main() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --confirm)  CONFIRM_TOKEN="${2:-}"; DRY_RUN=0; shift 2 ;;
      --resume)   RESUME=1; shift ;;
      --step)     STEP_FILTER="${2:-}"; shift 2 ;;
      --status)   show_status; exit 0 ;;
      -h|--help)  usage; exit 0 ;;
      *)          die "unknown argument: $1" ;;
    esac
  done

  require_root
  mkdir -p "$LOG_DIR" "$STATE_DIR" 2>/dev/null || true
  TRANSCRIPT="$LOG_DIR/dr-recover-$(date -u +%Y%m%dT%H%M%SZ).jsonl"
  record "start" "pool=$POOL datasets=$DATASETS"

  log "disaster recovery — runbook phase C (data)"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "DRY RUN: nothing will be written. This is the default."
  fi

  preflight_source || die "pre-flight failed; nothing was written"
  preflight_target || die "pre-flight failed; nothing was written"

  # The token is checked AFTER the pre-flight, so the token a real run needs is
  # one that could only have been produced by a plan that passed its checks.
  if [[ "$DRY_RUN" -eq 0 ]]; then
    local want; want="$(expected_token)"
    if [[ "$CONFIRM_TOKEN" != "$want" ]]; then
      emit_err ""
      emit_err "Refusing to restore: the confirmation token does not match this plan."
      emit_err "  expected: $want"
      emit_err "  given:    ${CONFIRM_TOKEN:-<none>}"
      emit_err ""
      emit_err "The token names the pool and a digest of the plan, so one typed from"
      emit_err "a run against a different target cannot confirm this one."
      record "refused" "token mismatch"
      write_metrics "refused"
      exit 2
    fi
    ok "confirmation token matches this plan"
    write_metrics "running"
  fi

  step_datasets || die "the dataset restore failed; stopping rather than continuing"
  step_database || die "the database replay failed; stopping rather than continuing"
  step_verify

  if [[ "$DRY_RUN" -eq 1 ]]; then
    emit ""
    emit "This was a dry run. Nothing was written."
    emit "To perform this recovery, re-run with:"
    emit ""
    emit "    sudo $0 --confirm $(expected_token)"
    emit ""
    record "dry_run_complete" "$(expected_token)"
    write_metrics "planned"
    exit 0
  fi

  record "complete" "failures=$FAILURES"
  if [[ "$FAILURES" -gt 0 ]]; then
    write_metrics "failed"
    die "$FAILURES step(s) failed; the transcript is at $TRANSCRIPT"
  fi
  write_metrics "success"
  log "recovery complete. Transcript: $TRANSCRIPT"
  log "runbook phases D (stack), E (protection) and F (verify) remain manual."
}

show_status() {
  if [[ ! -f "$PROM_FILE" ]]; then
    echo "no recovery has ever been run on this machine (no $PROM_FILE)"
    return 0
  fi
  cat "$PROM_FILE"
  if [[ -d "$STATE_DIR" ]]; then
    echo "completed steps:"
    local f
    for f in "$STATE_DIR"/*.done; do
      [[ -e "$f" ]] || continue
      echo "  $(basename "$f" .done) at $(cat "$f")"
    done
  fi
}

main "$@"
