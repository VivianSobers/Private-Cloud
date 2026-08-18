#!/usr/bin/env bash
#
# tailscale-cert.sh — issue and renew a real TLS certificate for the tailnet
# plane, replacing Caddy's internal CA.
#
#   sudo ./scripts/tailscale-cert.sh            # issue or renew, reload if changed
#   sudo ./scripts/tailscale-cert.sh --force    # renew even if not near expiry
#   sudo ./scripts/tailscale-cert.sh --status   # what is installed, and until when
#
# Why this exists: `tls internal` makes Caddy mint its own certificate, which
# every browser refuses until you trust that CA on every device. That is not
# merely inconvenient — it teaches you to click through certificate warnings on
# the one machine that holds all of your files.
#
# Tailscale issues a genuine Let's Encrypt certificate for
# <host>.<tailnet>.ts.net. Nothing is exposed to the internet to obtain it: the
# DNS-01 challenge is answered by Tailscale's nameservers, which is exactly why
# it works for a host with no public address and no forwarded port.
#
# Requires, in the tailnet admin console: MagicDNS enabled, and HTTPS
# Certificates enabled. See docs/tailscale-setup.md §4.
set -euo pipefail

CERT_DIR="${CERT_DIR:-/opt/private-cloud/deploy/caddy/certs}"
TLS_CONF="${TLS_CONF:-/opt/private-cloud/deploy/caddy/tls.conf}"
CADDY_CONTAINER="${CADDY_CONTAINER:-privatecloud-caddy}"
# Renew when fewer than this many days remain. Tailscale certificates are valid
# for 90 days; renewing at 30 leaves two further weekly attempts before expiry,
# so one failed run is not an outage.
RENEW_BEFORE_DAYS="${RENEW_BEFORE_DAYS:-30}"

# Paths inside the Caddy container, which is where tls.conf is read.
CONTAINER_CERT="/etc/caddy/certs/tailnet.crt"
CONTAINER_KEY="/etc/caddy/certs/tailnet.key"

log()  { printf '[tailscale-cert] %s\n' "$*"; }
warn() { printf '[tailscale-cert] WARNING: %s\n' "$*" >&2; }
die()  { printf '[tailscale-cert] ERROR: %s\n' "$*" >&2; exit 1; }

command -v tailscale >/dev/null 2>&1 || die "tailscale is not installed"

# ---------------------------------------------------------------------------
# Work out our own name. `tailscale status --json` is authoritative; guessing
# from the hostname gets the tailnet suffix wrong and issues a certificate for a
# name nothing resolves.
# ---------------------------------------------------------------------------
fqdn() {
  local name
  if command -v jq >/dev/null 2>&1; then
    name="$(tailscale status --json | jq -r '.Self.DNSName')"
  else
    name="$(tailscale status --json |
      tr ',' '\n' | grep -m1 '"DNSName"' | cut -d'"' -f4)"
  fi
  name="${name%.}"                      # DNSName carries a trailing dot
  [[ -n "$name" ]] || die "could not determine this machine's MagicDNS name — is MagicDNS enabled?"
  printf '%s\n' "$name"
}

days_left() {
  local crt="$1"
  [[ -f "$crt" ]] || { printf '%s\n' -1; return; }
  local end epoch now
  end="$(openssl x509 -in "$crt" -noout -enddate 2>/dev/null | cut -d= -f2)" || { printf '%s\n' -1; return; }
  epoch="$(date -d "$end" +%s 2>/dev/null)" || { printf '%s\n' -1; return; }
  now="$(date +%s)"
  printf '%s\n' $(( (epoch - now) / 86400 ))
}

case "${1:-}" in
  --status)
    name="$(fqdn)"
    log "machine:     $name"
    log "cert dir:    $CERT_DIR"
    if [[ -f "$CERT_DIR/tailnet.crt" ]]; then
      log "subject:     $(openssl x509 -in "$CERT_DIR/tailnet.crt" -noout -subject | sed 's/^subject=//')"
      log "issuer:      $(openssl x509 -in "$CERT_DIR/tailnet.crt" -noout -issuer | sed 's/^issuer=//')"
      log "expires in:  $(days_left "$CERT_DIR/tailnet.crt") days"
    else
      log "no certificate installed; Caddy is using its internal CA"
    fi
    grep -qs '^tls internal' "$TLS_CONF" && log "tls.conf:    internal CA" || log "tls.conf:    $(grep -m1 '^tls ' "$TLS_CONF" || echo 'unreadable')"
    exit 0
    ;;
  --force) FORCE=1 ;;
  "")      FORCE=0 ;;
  *)       die "unknown argument: $1 (expected --force or --status)" ;;
esac

NAME="$(fqdn)"
mkdir -p "$CERT_DIR"
chmod 0755 "$CERT_DIR"

remaining="$(days_left "$CERT_DIR/tailnet.crt")"
if [[ "$FORCE" -eq 0 && "$remaining" -ge "$RENEW_BEFORE_DAYS" ]]; then
  log "certificate for $NAME has $remaining days left; nothing to do"
  exit 0
fi

log "issuing certificate for $NAME (had ${remaining} days)"

# Write to temporary files first. A half-written certificate that Caddy reloads
# is a broken listener, and this runs unattended from a timer.
tmp_crt="$(mktemp)"; tmp_key="$(mktemp)"
trap 'rm -f "$tmp_crt" "$tmp_key"' EXIT

if ! tailscale cert --cert-file "$tmp_crt" --key-file "$tmp_key" "$NAME"; then
  die "tailscale cert failed — check that HTTPS Certificates are enabled for this tailnet"
fi

# Prove the pair matches before installing it. A mismatched key is the one
# failure mode that produces a listener which starts and then refuses every
# connection.
crt_mod="$(openssl x509 -noout -modulus -in "$tmp_crt" | openssl md5)"
key_mod="$(openssl rsa  -noout -modulus -in "$tmp_key" 2>/dev/null | openssl md5 || true)"
if [[ -n "$key_mod" && "$crt_mod" != "$key_mod" ]]; then
  die "issued certificate and key do not match; refusing to install"
fi

install -m 0644 "$tmp_crt" "$CERT_DIR/tailnet.crt"
install -m 0640 "$tmp_key" "$CERT_DIR/tailnet.key"
log "installed into $CERT_DIR (expires in $(days_left "$CERT_DIR/tailnet.crt") days)"

# Point Caddy at it. Rewriting rather than appending, so running this twice does
# not leave two tls directives in one file.
cat > "$TLS_CONF" <<EOF
# Managed by scripts/tailscale-cert.sh — edited automatically on renewal.
#
# Real Let's Encrypt certificate for $NAME, issued through Tailscale's DNS-01
# challenge. Nothing is exposed to the internet to obtain or renew it.
#
# Put \`tls internal\` back and reload to return to Caddy's own CA.
tls $CONTAINER_CERT $CONTAINER_KEY
EOF
log "wrote $TLS_CONF"

# Reload rather than restart: a reload keeps existing connections and, more to
# the point, a bad config fails the reload and leaves the running server alone.
if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -qx "$CADDY_CONTAINER"; then
  if docker exec "$CADDY_CONTAINER" caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
    log "reloaded $CADDY_CONTAINER"
  else
    warn "caddy reload failed; the certificate is installed but not yet live"
    warn "check: docker exec $CADDY_CONTAINER caddy validate --config /etc/caddy/Caddyfile"
    exit 1
  fi
else
  log "$CADDY_CONTAINER is not running; the new certificate is installed and will be used on next start"
fi
