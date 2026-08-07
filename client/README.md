# pcsync — Private Cloud sync client

A headless daemon that keeps one local folder in step with your Private Cloud
server, in both directions, over the block-level delta protocol. Edit a file on
your laptop and it appears on your desktop within seconds; a 4 GB file that
changed by one block transfers one block, not four gigabytes.

This is a **separate Go module** from `server/`, on purpose. It ships to laptops
and desktops, so it builds with a pure-Go toolchain (no CGO, no C compiler) and
drags in none of the server's database or auth dependencies. The only thing it
shares with the server is the wire protocol — and a protocol is a contract, not
shared code, so the FastCDC + BLAKE3 chunking parameters are re-declared here to
match rather than imported across the boundary.

## How it works

The server stays authoritative. The client never talks to another client; it
reconciles its local folder against the server, and two devices converge because
they both converge on the server — which keeps sync a two-party problem instead
of an n-party consensus one.

- **Local state database** (SQLite, pure-Go) records, per path, the version it
  last synced: the server's content hash, plus the local file's size and mtime.
  This baseline — never the filesystem clock alone, which lies across restores —
  is what both local edits and remote changes are judged against.
- **Initial sync** walks the server tree, materializes it locally, and records
  the change-journal head as its starting cursor.
- **Steady state** is two loops plus a safety net: apply remote changes pulled
  from the journal (`GET /changes`), push local changes detected by watching the
  filesystem (fsnotify), and a slower full rescan that catches any event the
  watcher missed by comparing against real file hashes.
- **Uploads** run the delta protocol: chunk locally, ask the server which chunks
  it already has, upload only the new ones, commit a manifest. Files below the
  chunking threshold are sent whole.
- **Every downloaded chunk is verified** against its address before it is
  trusted — the client trusts the server's bytes exactly as much as the server
  trusts the client's, which is to say only after checking.

Conflict handling is by lineage, never by clocks: when a file changed on *both*
sides since it was last synced, the server's version keeps the original name and
the local edit is set aside as a visible *conflict copy* —
`name (conflict from HOST DATE).ext` — then uploaded as its own file. Nothing is
overwritten and nothing is silently merged; you resolve it by choosing between
two files, a decision a person can make and an automatic merge cannot.

## Setup

1. On the server, mint an app password (web UI → app passwords, or
   `cloudctl user app-password <username> <name>`). It looks like `pcap_<lookup>_<secret>`
   and is shown once.
2. Copy `config.example.json`, fill in your server URL, username, the app
   password, and the local folder to sync:

   ```json
   {
     "server_url": "https://cloud.example.ts.net",
     "username": "vivian",
     "app_password": "pcap_…",
     "root": "/home/vivian/PrivateCloud"
   }
   ```

3. Run it:

   ```bash
   go build -o pcsync ./cmd/pcsync
   ./pcsync -config ./config.json          # stay resident
   ./pcsync -config ./config.json -once    # one reconcile, then exit (cron, "sync now")
   ```

The app password is exchanged for a short-lived **device token** on first
contact; the password itself never touches the file endpoints. A device token
can read and write your files but cannot manage account credentials — it cannot
mint another app password, the same limit the app password itself has.

## Run as a service (systemd)

To keep syncing in the background across logins, install the provided **user**
service — it runs as you, with your credential, syncing your folder:

```bash
go build -o ~/.local/bin/pcsync ./cmd/pcsync
mkdir -p ~/.config/systemd/user ~/.config/pcsync
cp client/deploy/pcsync.service ~/.config/systemd/user/
cp client/config.example.json   ~/.config/pcsync/config.json   # then edit it
systemctl --user daemon-reload
systemctl --user enable --now pcsync
journalctl --user -u pcsync -f                                 # watch it
```

On a headless box or a desktop that logs you out, let the service run without an
active session:

```bash
sudo loginctl enable-linger "$USER"
```

The unit hardens the daemon (no new privileges, `/usr` and `/etc` read-only,
private `/tmp`) while leaving your home tree writable — which is where both the
synced folder and the state database live.

## Watch and control it

The resident daemon serves a local **control socket** — a Unix socket at
`<state>/control.sock`, never a network port, gated by owner-only file
permissions — so you can watch and steer it without reading logs. The same
`pcsync` binary is the client:

```bash
pcsync status -config ./config.json     # up to date, syncing, paused, or broken?
pcsync sync   -config ./config.json     # reconcile now (works even while paused)
pcsync pause  -config ./config.json     # stop automatic syncing (e.g. on a hotspot)
pcsync resume -config ./config.json     # resume it
```

`status` reports the current state, how many items are tracked, when the last
sync succeeded, any lingering error, and the list of conflict copies that need
your attention:

```
pcsync 1.0 — https://cloud.example.ts.net
  folder:     /home/vivian/PrivateCloud
  state:      idle
  tracked:    1284 items
  last sync:  8s ago
  conflicts:  1 need attention
    /notes/todo.txt  →  /notes/todo (conflict from laptop 2026-08-07).txt
```

`pause` stops the automatic poll/rescan/watch cadences; an explicit `sync` still
runs, so paused never means stuck. This control surface is also the contract a
desktop tray app drives — the GUI is a thin shell over these same calls.

## Selective sync

A small laptop need not carry your whole cloud. **Excludes** are server-path
prefixes this device declines to sync — the folders never download here, and
their absence never deletes them on the server. Exclusion is a per-device, local
decision; every other device (and the server) keeps the complete tree.

Seed it in the config (`"excludes": ["/Videos"]`) or manage it live against the
running daemon:

```bash
pcsync exclude list           -config ./config.json   # what's excluded here
pcsync exclude add    /Videos  -config ./config.json   # stop syncing /Videos
pcsync exclude remove /Videos  -config ./config.json   # start syncing it again
```

Live changes persist in the local state database and survive restarts, so the
config's `excludes` is only the **first-run seed** — once you've adjusted the set
through `pcsync exclude`, that choice wins. Adding an exclusion for a folder that
is already synced prunes it locally to reclaim the space; if that folder holds a
local edit that hasn't been pushed yet, the files are left on disk untouched (they
simply stop syncing) rather than risk losing the edit.

## Configuration

| Key | Meaning | Default |
|---|---|---|
| `server_url` | Base URL of the API | required |
| `username` | Account username (checked against the app password) | required |
| `app_password` | A `pcap_` credential | required |
| `root` | Local folder to mirror | required |
| `state_db` | Where the local sync database lives | `<root>/.pcsync/state.db` |
| `poll_seconds` | How often the change journal is polled | 15 |
| `rescan_seconds` | How often a full local rescan runs | 300 |
| `excludes` | Server-path prefixes this device does not sync | `[]` (whole tree) |

The `.pcsync` directory under the root holds the state database, download temp
files, and the control socket, and is never synced.
