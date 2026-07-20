#!/usr/bin/env bash
#
# zpool-metrics.sh — node_exporter textfile collector for ZFS pool health.
#
#   Usage:
#     sudo TEXTFILE_DIR=/var/lib/node_exporter/textfile ./zpool-metrics.sh
#     sudo POOL=tank ./zpool-metrics.sh        # restrict to one pool (validation)
#
# Run from a systemd timer every few minutes — see
# deploy/systemd/privatecloud-zpool-metrics.{service,timer}. node_exporter's ZFS
# collector reports ARC and IO but NOT `zpool status`, so pool DEGRADED/FAULTED
# and scrub freshness are invisible without this. That is the gap it closes.
#
# Metrics written:
#   privatecloud_zpool_health{pool,state}             1 for the pool's current state, 0 for the rest
#   privatecloud_zpool_scrub_age_seconds{pool}        age of last completed scrub (or since creation if never)
#   privatecloud_zpool_last_scrub_success{pool}       1 = last scrub found 0 errors; 0 = errors; absent if never scrubbed
#   privatecloud_zpool_metrics_last_update_timestamp  when this collector last ran (staleness detection)
#
set -euo pipefail

TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile}"
OUT="$TEXTFILE_DIR/privatecloud_zpool.prom"

# Every pool health state we distinguish. The current state is emitted as 1 and
# all others as 0, so an alert matching state="DEGRADED" is unambiguous and no
# series goes stale across a state transition.
STATES=(ONLINE DEGRADED FAULTED OFFLINE UNAVAIL REMOVED SUSPENDED)

now="$(date +%s)"
mkdir -p "$TEXTFILE_DIR"

# Which pools to report on. Default: every imported pool (so a new pool is
# picked up automatically and a destroyed one drops out on the next run).
pools=()
if [[ -n "${POOL:-}" ]]; then
  pools=("$POOL")
else
  mapfile -t pools < <(zpool list -H -o name 2>/dev/null || true)
fi

# Accumulate one block per metric family so samples stay grouped under a single
# HELP/TYPE, which the Prometheus text parser expects.
health_lines=""
age_lines=""
success_lines=""

for pool in "${pools[@]}"; do
  [[ -n "$pool" ]] || continue

  # ---- health ----
  health="$(zpool list -H -o health "$pool" 2>/dev/null || echo UNAVAIL)"
  [[ -n "$health" ]] || health=UNAVAIL
  for s in "${STATES[@]}"; do
    if [[ "$s" == "$health" ]]; then
      health_lines+="privatecloud_zpool_health{pool=\"$pool\",state=\"$s\"} 1"$'\n'
    else
      health_lines+="privatecloud_zpool_health{pool=\"$pool\",state=\"$s\"} 0"$'\n'
    fi
  done

  # ---- scrub ----
  # Parse the single "scan:" line from `zpool status`. A completed scrub reads:
  #   scrub repaired 0B in 00:00:03 with 0 errors on Sun Jul 20 04:00:01 2026
  status="$(zpool status "$pool" 2>/dev/null || true)"
  scan="$(printf '%s\n' "$status" | sed -n 's/^[[:space:]]*scan:[[:space:]]*//p' | head -1)"

  scrub_end_epoch=""
  scrub_success=""
  if [[ "$scan" =~ scrub\ repaired.*with\ ([0-9]+)\ errors\ on\ (.+)$ ]]; then
    errs="${BASH_REMATCH[1]}"
    when="${BASH_REMATCH[2]}"
    scrub_end_epoch="$(date -d "$when" +%s 2>/dev/null || echo "")"
    if [[ "$errs" == "0" ]]; then scrub_success=1; else scrub_success=0; fi
  fi

  if [[ -n "$scrub_end_epoch" ]]; then
    age_lines+="privatecloud_zpool_scrub_age_seconds{pool=\"$pool\"} $(( now - scrub_end_epoch ))"$'\n'
  else
    # Never scrubbed: age from pool creation, so a pool that is never scrubbed
    # still trips ZpoolScrubTooOld instead of looking permanently fresh.
    creation="$(zpool get -Hp -o value creation "$pool" 2>/dev/null || echo "$now")"
    [[ "$creation" =~ ^[0-9]+$ ]] || creation="$now"
    age_lines+="privatecloud_zpool_scrub_age_seconds{pool=\"$pool\"} $(( now - creation ))"$'\n'
  fi

  # Emit success only when a scrub has actually completed, so ZpoolScrubFailed
  # can't false-fire on a pool that has simply never been scrubbed.
  if [[ -n "$scrub_success" ]]; then
    success_lines+="privatecloud_zpool_last_scrub_success{pool=\"$pool\"} $scrub_success"$'\n'
  fi
done

# --------------------------------------------------------------------------
# render + atomic write
# --------------------------------------------------------------------------
tmp="$(mktemp "${OUT}.XXXXXX")"
{
  echo "# HELP privatecloud_zpool_health ZFS pool health; value 1 marks the pool's current state."
  echo "# TYPE privatecloud_zpool_health gauge"
  printf '%s' "$health_lines"

  echo "# HELP privatecloud_zpool_scrub_age_seconds Seconds since the last completed scrub (or pool creation if never scrubbed)."
  echo "# TYPE privatecloud_zpool_scrub_age_seconds gauge"
  printf '%s' "$age_lines"

  echo "# HELP privatecloud_zpool_last_scrub_success 1 if the last completed scrub found zero errors, 0 otherwise."
  echo "# TYPE privatecloud_zpool_last_scrub_success gauge"
  printf '%s' "$success_lines"

  echo "# HELP privatecloud_zpool_metrics_last_update_timestamp Unix time this collector last wrote its metrics."
  echo "# TYPE privatecloud_zpool_metrics_last_update_timestamp gauge"
  echo "privatecloud_zpool_metrics_last_update_timestamp $now"
} > "$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$OUT"
