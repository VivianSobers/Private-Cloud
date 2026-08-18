# Custom metrics & operational hardening

**Status: ✅ both collectors, all alerts and the failure simulations are built and
rule-tested.** 🟠 What is unticked is operator work: the simulations in
§Validation have to be run once on the real server, and ❌ the Grafana dashboards
are still not exported to JSON and committed —
`deploy/monitoring/grafana/dashboards/` holds only `.gitkeep`. The same numbers
these collectors write are what `GET /admin/storage` reads, by design, so the
console and Grafana cannot disagree.

Phase 0's stock exporters cover host, container, disk-SMART, and Postgres
metrics. Two critical failure modes have no stock coverage, so we export them
ourselves through node_exporter's **textfile collector**:

1. **Backup freshness** — stock alerting knows if the backup *timer* unit is
   inactive, but not whether backups are actually *succeeding*. A repo that has
   silently produced nothing for weeks passes every other check.
2. **ZFS pool health** — node_exporter's ZFS collector reports ARC and IO but
   **not** `zpool status`, so a `DEGRADED`/`FAULTED` pool or a stale/failed
   scrub is invisible to it.

Both are wired into the existing Prometheus → Alertmanager → ntfy path by
severity label — no Alertmanager config change is needed.

---

## How the textfile collector is wired

- Host scripts write `*.prom` files into `/var/lib/node_exporter/textfile`.
- node_exporter reads that directory through its `/host` bind mount
  (`--collector.textfile.directory=/host/var/lib/node_exporter/textfile`), so
  no extra Docker mount is required.
- Both writers write to a temp file and `mv` into place, so node_exporter never
  scrapes a half-written file.
- The metrics ride the existing `node` scrape job — **no new scrape target.**

Create the directory before starting the stack:

```bash
sudo mkdir -p /var/lib/node_exporter/textfile
sudo chmod 0755 /var/lib/node_exporter/textfile
```

---

## Metrics

### Backup freshness — `scripts/restic-backup.sh` (written after every run)

| Metric | Meaning |
|---|---|
| `privatecloud_backup_last_success_timestamp` | Unix time of the last successful backup |
| `privatecloud_backup_last_failure_timestamp` | Unix time of the last failed backup |
| `privatecloud_backup_last_run_success` | 1 if the most recent run succeeded, else 0 |
| `privatecloud_backup_age_seconds` | **Recording rule** (`alerts.yml`): `time() - last_success_timestamp`. Derived live, not written to the file, because a written age would freeze at ~0. |

### ZFS pool health — `scripts/zpool-metrics.sh` (systemd timer, every 5 min)

| Metric | Meaning |
|---|---|
| `privatecloud_zpool_health{pool,state}` | One series per state (`ONLINE`, `DEGRADED`, `FAULTED`, `OFFLINE`, `UNAVAIL`, `REMOVED`, `SUSPENDED`); value `1` marks the current state |
| `privatecloud_zpool_scrub_age_seconds{pool}` | Seconds since the last completed scrub (or since pool creation if never scrubbed) |
| `privatecloud_zpool_last_scrub_success{pool}` | 1 if the last scrub found 0 errors, 0 if it found errors; **absent** until a scrub has ever completed |
| `privatecloud_zpool_metrics_last_update_timestamp` | When the collector last ran, for staleness detection |

---

## Alerts (all in `alerts.yml`, tested in `alerts_test.yml`)

| Alert | Severity | Fires when |
|---|---|---|
| `BackupTooOld` | critical | `privatecloud_backup_age_seconds > 129600` (36h) for 30m |
| `BackupMetricsMissing` | warning | `absent(...last_success_timestamp)` for 6h |
| `BackupLastRunFailed` | warning | failure timestamp newer than success timestamp |
| `ZpoolDegraded` | warning | `health{state="DEGRADED"} == 1` for 5m |
| `ZpoolUnavailable` | critical | `health{state=~"FAULTED\|UNAVAIL\|REMOVED\|SUSPENDED"} == 1` for 1m |
| `ZpoolScrubTooOld` | warning | `scrub_age_seconds > 3024000` (~5 weeks) for 1h |
| `ZpoolScrubFailed` | critical | `last_scrub_success == 0` for 5m |
| `ZpoolMetricsStale` | warning | collector hasn't updated in 15 min |
| `ZpoolMetricsMissing` | warning | collector has never reported |

Thresholds live in `alerts.yml`. To change the backup RPO, edit the `129600`
(seconds) in `BackupTooOld`. The unit tests run offline:

```bash
docker run --rm -v "$PWD/deploy/monitoring:/cfg:ro" -w /cfg \
  --entrypoint promtool prom/prometheus:v3.1.0 test rules alerts_test.yml
```

---

## Validation — simulate each failure and watch the full chain

Set `TS` to your Tailscale IP first. Queries go through Caddy (`-k` for the
internal CA). Give Prometheus one scrape/eval cycle (~30–60s) after each change.

### 1. Stale backup metric → `BackupTooOld`

```bash
# Back-date the last-success timestamp to 40h ago:
f=/var/lib/node_exporter/textfile/privatecloud_backup.prom
sudo sed -i "s/^privatecloud_backup_last_success_timestamp .*/privatecloud_backup_last_success_timestamp $(( $(date +%s) - 144000 ))/" "$f"

# Confirm the derived age crosses the threshold:
curl -sk "https://$TS/prometheus/api/v1/query?query=privatecloud_backup_age_seconds" | jq '.data.result[0].value[1]'
#   expect a number > 129600

# After ~30m of `for`, or check the pending state immediately:
curl -sk "https://$TS/prometheus/api/v1/alerts" | jq -r '.data.alerts[] | select(.labels.alertname=="BackupTooOld") | .state'

# RESTORE truth — run a real backup (rewrites the metric):
sudo ./scripts/restic-backup.sh backup
```

### 2. Degraded / unavailable pool → `ZpoolDegraded` / `ZpoolUnavailable`

Two ways. **(a) Chain test — inject the metric** (works on any hardware; tests
Prometheus→Alertmanager→ntfy). Stop the timer first so it doesn't overwrite:

```bash
sudo systemctl stop privatecloud-zpool-metrics.timer
sudo tee /var/lib/node_exporter/textfile/privatecloud_zpool.prom >/dev/null <<EOF
privatecloud_zpool_health{pool="tank",state="ONLINE"} 0
privatecloud_zpool_health{pool="tank",state="DEGRADED"} 1
privatecloud_zpool_metrics_last_update_timestamp $(date +%s)
EOF
# watch ZpoolDegraded go pending→firing, and the ntfy push land:
curl -sk "https://$TS/prometheus/api/v1/alerts" | jq -r '.data.alerts[] | select(.labels.alertname|startswith("Zpool")) | "\(.labels.alertname) \(.state)"'
# RESTORE:
sudo systemctl start privatecloud-zpool-metrics.timer   # next run rewrites the truth
```

**(b) Collector-logic test — real fault on a throwaway mirror** (proves the
script parses `zpool status` correctly, without touching `tank`):

```bash
truncate -s 256M /tmp/d1.img /tmp/d2.img
sudo zpool create testpool mirror /tmp/d1.img /tmp/d2.img
sudo zpool offline testpool /tmp/d1.img            # → DEGRADED
sudo ./scripts/zpool-metrics.sh                    # picks up ALL pools incl. testpool
grep testpool /var/lib/node_exporter/textfile/privatecloud_zpool.prom
#   expect state="DEGRADED" ... 1
sudo zpool destroy testpool && rm -f /tmp/d1.img /tmp/d2.img
sudo ./scripts/zpool-metrics.sh                    # testpool drops out next run
```

### 3. Failed scrub → `ZpoolScrubFailed`

Corrupt a *throwaway single-disk* pool so a scrub finds unrepairable errors —
never do this to `tank`:

```bash
truncate -s 512M /tmp/scrub-test.img
sudo zpool create scrubtest /tmp/scrub-test.img
echo hello | sudo tee /scrubtest/data.txt >/dev/null
sudo zpool export scrubtest
sudo dd if=/dev/urandom of=/tmp/scrub-test.img bs=1M seek=40 count=80 conv=notrunc
sudo zpool import -d /tmp scrubtest
sudo zpool scrub -w scrubtest                      # -w waits for completion
zpool status scrubtest | grep scan                 # → "with N errors"
sudo ./scripts/zpool-metrics.sh
grep 'last_scrub_success{pool="scrubtest"}' /var/lib/node_exporter/textfile/privatecloud_zpool.prom
#   expect value 0 → ZpoolScrubFailed fires
sudo zpool destroy scrubtest && rm -f /tmp/scrub-test.img
sudo ./scripts/zpool-metrics.sh
```

### 4. Collector stopped → `ZpoolMetricsStale`

```bash
sudo systemctl stop privatecloud-zpool-metrics.timer
# wait ~15 min; the last-update timestamp goes stale:
curl -sk "https://$TS/prometheus/api/v1/alerts" | jq -r '.data.alerts[] | select(.labels.alertname=="ZpoolMetricsStale") | .state'
sudo systemctl start privatecloud-zpool-metrics.timer
```

After every simulation, confirm the alert **resolved** (Alertmanager sends a
`resolved` push with `send_resolved: true`) so you know the recovery path works
too, not just the firing path.
