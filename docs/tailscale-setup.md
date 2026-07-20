# Tailscale Setup

Tailscale is the backbone of the private plane. Once it's up, **zero ports are
forwarded on your router** and the entire stack is reachable only from devices
you've explicitly authorised.

> These commands run **on the Ubuntu server**. Nothing here can be executed
> from the Windows machine where this repo was authored.

---

## 1. Install

```bash
curl -fsSL https://tailscale.com/install.sh | sh
```

This adds Tailscale's apt repo and installs the package, so it upgrades with
the rest of the system.

## 2. Bring it up

```bash
sudo tailscale up --hostname=cloud --ssh
```

- `--hostname=cloud` sets the MagicDNS name, so the server is `cloud` on your
  tailnet rather than whatever the machine's hostname happens to be. This value
  must match `TS_HOSTNAME` in `.env`.
- `--ssh` lets you SSH in using Tailscale identity instead of managing keys and
  an exposed `sshd`. Optional but genuinely useful — you can drop port 22 from
  the LAN entirely.

The command prints a URL. Open it, log in, and the machine joins your tailnet.

### Disable key expiry

By default Tailscale keys expire after ~6 months, and when they do, the server
silently drops off the tailnet — including your backup target and your only
route in. In the admin console (`login.tailscale.com` → Machines → `cloud` →
⋯ → **Disable key expiry**), turn it off for the server.

This is a real tradeoff: a permanently-valid key is a permanently-valid key. It
is the right call for an always-on server you physically control, and the wrong
call for a laptop.

## 3. Get the values for `.env`

```bash
tailscale ip -4                                  # -> TAILSCALE_IP
tailscale status --json | jq -r .MagicDNSSuffix  # -> TS_TAILNET
```

Put them in `deploy/compose/.env`:

```
TAILSCALE_IP=100.x.y.z
TS_HOSTNAME=cloud
TS_TAILNET=your-tailnet.ts.net
```

`TAILSCALE_IP` is what every published port binds to. Compose is written to
**fail loudly if it's unset**, because the alternative — silently binding
`0.0.0.0` and exposing Grafana to the LAN — is exactly the mistake this design
exists to prevent.

## 4. Enable MagicDNS and HTTPS

In the admin console → **DNS**:

1. Enable **MagicDNS** — devices resolve each other by name.
2. Enable **HTTPS Certificates**.

With HTTPS enabled you can get a real, publicly-trusted Let's Encrypt
certificate for `cloud.your-tailnet.ts.net` without exposing anything:

```bash
sudo tailscale cert cloud.your-tailnet.ts.net
```

This is strictly better than the `tls internal` the Caddyfile ships with, since
it removes the browser warning without you having to distribute a private CA to
every device. To use it, replace `tls internal` in
[deploy/caddy/Caddyfile](../deploy/caddy/Caddyfile) with the cert paths and
mount them into the container. Ship with `tls internal` first — get the stack
working, then improve the certificate story.

## 5. Install on your devices

Install Tailscale on your laptop and phone and sign into the same tailnet. Both
should appear in the admin console.

## 6. Verify

From **another device on the tailnet** (not the server):

```bash
tailscale status              # server 'cloud' should be listed and online
ping cloud                    # MagicDNS resolution works
curl -k https://cloud/        # the Caddy landing page (once the stack is up)
```

`-k` skips certificate verification, which you need while Caddy is using its
internal CA. If this returns the landing page HTML, the private plane works.

### Verify it is NOT exposed

Just as important. From the server:

```bash
sudo ss -tlnp | grep -E ':(80|443|3000|9090|5432)'
```

Every listener should be bound to `100.x.y.z:port`, **not** `0.0.0.0:port` or
`*:port`. If you see `0.0.0.0`, `TAILSCALE_IP` was unset or wrong — stop and fix
it before going further.

---

## Firewall

Tailscale is the boundary, but defence in depth means the host firewall should
agree with that:

```bash
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow in on tailscale0          # trust the tailnet interface
sudo ufw allow 41641/udp                 # Tailscale's direct-connection port
sudo ufw allow from 192.168.0.0/16 to any port 22 proto tcp  # LAN SSH escape hatch
sudo ufw enable
```

Keep that last rule until you are confident Tailscale SSH works. Locking
yourself out of a headless server means walking over with a keyboard and
monitor — and if the pool is encrypted and unmounted, an unhappy afternoon.

## Related

- [phase-0-checklist.md](phase-0-checklist.md) — where this fits in the sequence
- [runbook-disaster-recovery.md](runbook-disaster-recovery.md) — rebuilding when the server is gone
