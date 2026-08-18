#!/usr/bin/env bash
#
# host-setup.sh — the Phase 0 hardening follow-ups, as code.
#
#   sudo ./scripts/host-setup.sh --all
#   sudo ./scripts/host-setup.sh --upgrades      # unattended security upgrades
#   sudo ./scripts/host-setup.sh --ups           # NUT, for a clean shutdown
#   sudo ./scripts/host-setup.sh --timers        # systemd units from deploy/systemd
#   sudo ./scripts/host-setup.sh --check         # report state, change nothing
#
# These lived in docs/phase-0-checklist.md as a list of things worth doing that
# never blocked a phase, and so were never done. A checklist item that survives
# nine phases is not a checklist item, it is a script nobody wrote.
#
# Everything here is idempotent: run it twice and the second run reports "already
# configured" rather than doubling anything.
set -euo pipefail

REPO_DIR="${REPO_DIR:-/opt/private-cloud}"
HOST_DIR="$REPO_DIR/deploy/host"
SYSTEMD_DIR="$REPO_DIR/deploy/systemd"

green() { printf '\033[32m%s\033[0m\n' "$*"; }
warn()  { printf '\033[33m[host-setup] WARNING: %s\033[0m\n' "$*" >&2; }
die()   { printf '\033[31m[host-setup] ERROR: %s\033[0m\n' "$*" >&2; exit 1; }
log()   { printf '[host-setup] %s\n' "$*"; }

[[ $EUID -eq 0 ]] || die "run as root (this installs into /etc)"
[[ -d "$HOST_DIR" ]] || die "$HOST_DIR not found — set REPO_DIR to the checkout"

# ---------------------------------------------------------------------------
install_upgrades() {
  log "configuring unattended security upgrades"
  DEBIAN_FRONTEND=noninteractive apt-get install -y unattended-upgrades apt-listchanges >/dev/null

  install -m 0644 "$HOST_DIR/apt/20auto-upgrades"      /etc/apt/apt.conf.d/20auto-upgrades
  install -m 0644 "$HOST_DIR/apt/50unattended-upgrades" /etc/apt/apt.conf.d/50unattended-upgrades

  # The dry run is the point of this function. "I installed unattended-upgrades"
  # and "security updates are being applied" are different claims, and only one
  # of them is checkable.
  if unattended-upgrade --dry-run --debug >/tmp/uu-dryrun.log 2>&1; then
    green "  unattended-upgrades: configured, dry run clean"
  else
    warn "unattended-upgrade --dry-run failed; see /tmp/uu-dryrun.log"
  fi
  systemctl enable --now unattended-upgrades.service >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
install_ups() {
  log "configuring NUT for clean shutdown on power loss"
  DEBIAN_FRONTEND=noninteractive apt-get install -y nut nut-client nut-server >/dev/null

  if ! lsusb 2>/dev/null | grep -qiE 'ups|american power|cyberpower|eaton|tripp'; then
    warn "no UPS detected on USB — installing the config anyway, but check with: nut-scanner -U"
  fi

  # One generated password, written into both files. Neither carries a secret in
  # git; both need the same one, and a mismatch is a upsmon that authenticates
  # against nothing and silently never shuts the machine down.
  local pw
  pw="$(openssl rand -base64 24 | tr -d '/+=' | head -c 24)"

  install -m 0644 "$HOST_DIR/nut/nut.conf"  /etc/nut/nut.conf
  install -m 0644 "$HOST_DIR/nut/ups.conf"  /etc/nut/ups.conf
  install -m 0644 "$HOST_DIR/nut/upsd.conf" /etc/nut/upsd.conf

  sed "s|UPSMON_PASSWORD_PLACEHOLDER|$pw|" "$HOST_DIR/nut/upsd.users" > /etc/nut/upsd.users
  sed "s|UPSMON_PASSWORD_PLACEHOLDER|$pw|" "$HOST_DIR/nut/upsmon.conf" > /etc/nut/upsmon.conf
  chown root:nut /etc/nut/upsd.users /etc/nut/upsmon.conf
  chmod 0640     /etc/nut/upsd.users /etc/nut/upsmon.conf

  install -m 0755 "$HOST_DIR/nut/privatecloud-ups-notify" /usr/local/sbin/privatecloud-ups-notify

  systemctl restart nut-server nut-monitor 2>/dev/null || systemctl restart nut-driver-enumerator nut-server nut-monitor
  sleep 2
  if upsc ups 2>/dev/null | grep -q 'battery.charge'; then
    green "  NUT: talking to the UPS ($(upsc ups battery.charge 2>/dev/null)% charge)"
  else
    warn "upsc could not read the UPS; check 'upsdrvctl start' and the driver in ups.conf"
  fi
}

# ---------------------------------------------------------------------------
install_timers() {
  log "installing systemd units"
  install -m 0644 "$SYSTEMD_DIR"/*.service /etc/systemd/system/
  install -m 0644 "$SYSTEMD_DIR"/*.timer   /etc/systemd/system/
  systemctl daemon-reload

  # Enabled: backups, integrity checks, the metrics collectors, the certificate
  # renewal. NOT enabled: privatecloud-zfs-unlock, which is opt-in and asks you
  # to decide where the key lives first.
  systemctl enable --now \
    restic-backup.timer \
    restic-check.timer \
    privatecloud-zpool-metrics.timer \
    privatecloud-tailscale-cert.timer \
    privatecloud-restore-drill.timer \
    privatecloud-pgbackrest-full.timer \
    privatecloud-pgbackrest-diff.timer >/dev/null
  green "  timers: $(systemctl list-timers --no-pager 'restic*' 'privatecloud-*' | grep -c privatecloud\\\|restic) scheduled"
  log "  privatecloud-zfs-unlock.service left disabled — see the comment in the unit"
}

# ---------------------------------------------------------------------------
check() {
  printf '\n%-34s %s\n' "CHECK" "STATE"
  printf '%-34s ' "unattended-upgrades"
  if [[ -f /etc/apt/apt.conf.d/50unattended-upgrades ]] && systemctl is-enabled --quiet unattended-upgrades 2>/dev/null; then
    green "configured"; else echo "not configured"; fi

  printf '%-34s ' "UPS (NUT)"
  if upsc ups >/dev/null 2>&1; then green "reachable"; else echo "not configured"; fi

  printf '%-34s ' "TLS certificate"
  if [[ -f "$REPO_DIR/deploy/caddy/certs/tailnet.crt" ]]; then
    green "installed ($(openssl x509 -in "$REPO_DIR/deploy/caddy/certs/tailnet.crt" -noout -enddate | cut -d= -f2))"
  else echo "internal CA"; fi

  printf '%-34s ' "pool auto-unlock"
  if systemctl is-enabled --quiet privatecloud-zfs-unlock 2>/dev/null; then
    echo "ENABLED (key on disk — read the unit)"; else green "disabled (passphrase at boot)"; fi

  printf '%-34s ' "point-in-time recovery"
  if docker exec -u postgres privatecloud-postgres pgbackrest --stanza=privatecloud check >/dev/null 2>&1; then
    green "archiving"; else echo "not configured (scripts/pgbackrest.sh setup)"; fi

  for t in restic-backup restic-check privatecloud-zpool-metrics privatecloud-tailscale-cert \
           privatecloud-restore-drill privatecloud-pgbackrest-full privatecloud-pgbackrest-diff; do
    printf '%-34s ' "$t.timer"
    if systemctl is-enabled --quiet "$t.timer" 2>/dev/null; then green "enabled"; else echo "not enabled"; fi
  done
  echo
}

# ---------------------------------------------------------------------------
[[ $# -gt 0 ]] || { grep -m20 '^#' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
for arg in "$@"; do
  case "$arg" in
    --all)      install_upgrades; install_ups; install_timers; check ;;
    --upgrades) install_upgrades ;;
    --ups)      install_ups ;;
    --timers)   install_timers ;;
    --check)    check ;;
    *)          die "unknown argument: $arg" ;;
  esac
done
