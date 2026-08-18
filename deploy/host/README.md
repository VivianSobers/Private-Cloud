# Host configuration

Configuration that belongs to the **machine** rather than to a container: power
management, unattended security updates. Everything here is installed by
[`scripts/host-setup.sh`](../../scripts/host-setup.sh), which is idempotent — run
it twice and the second run changes nothing.

```bash
sudo ./scripts/host-setup.sh --all      # everything below
sudo ./scripts/host-setup.sh --check    # report state, change nothing
```

These items lived in `docs/phase-0-checklist.md` for nine phases as a list of
things "worth doing, not worth blocking Phase 1 on". That framing is honest and
it is also why none of them happened: a list nothing blocks on is a list nothing
gets removed from. They are files now.

## `apt/` — unattended security upgrades

Security origins only. Feature updates are not applied unattended, and four
packages are blacklisted outright:

| Blacklisted | Because |
|---|---|
| `docker-ce`, `docker-ce-cli`, `containerd.io` | an engine upgrade restarts every container mid-write |
| `tailscale` | losing the tailnet is losing the only route to this machine |
| `zfs-dkms`, `zfsutils-linux`, `linux-image-generic` | a kernel and a ZFS module that disagree produce a machine that boots *without its pool*, which looks exactly like having lost it |

`Automatic-Reboot` is off. The pool is encrypted and does not auto-unlock, so an
unattended 6am reboot is an outage that lasts until somebody types a passphrase.

`host-setup.sh --upgrades` runs `unattended-upgrade --dry-run` after installing,
because "I installed unattended-upgrades" and "security updates are being
applied" are different claims and only one of them is checkable.

## `nut/` — UPS monitoring

The point is **not uptime**. ZFS survives a power cut by design; what it survives
badly is the second and third power cut during the resilver after the first. A
UPS here buys a clean shutdown, every time, and nothing else.

- `upsd` listens on loopback only. Exposing it on the tailnet would add a
  network surface whose sole purpose is letting another machine shut this one
  down.
- The `upsmon` password is generated at install time and written into both
  `upsd.users` and `upsmon.conf`, so neither carries a secret in git and the two
  cannot drift apart. A mismatch is a `upsmon` that authenticates against nothing
  and then silently never shuts the machine down.
- `privatecloud-ups-notify` pushes power events to the same ntfy topics as every
  other alert. `ONBATT` and `LOWBATT` go to the critical topic: everything else
  on this server can wait, but the machine is about to switch itself off.
- `FINALDELAY` is left at its default on purpose. The temptation is to buy a few
  more minutes on battery, which spends exactly the margin that makes the
  shutdown clean.

Check the driver for your unit with `nut-scanner -U` before trusting `usbhid-ups`.

## What is deliberately not here

**Pool auto-unlock.** [`privatecloud-zfs-unlock.service`](../systemd/privatecloud-zfs-unlock.service)
exists and is not enabled. Read its header before enabling it: how much you give
up depends entirely on where the keyfile lives, and one popular answer — a key
on the root filesystem — makes the encryption decorative, so
[`scripts/zfs-unlock.sh`](../../scripts/zfs-unlock.sh) refuses it.
