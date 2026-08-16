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

Not syncing and not sure why? Run the preflight — it works without the daemon and
diagnoses the usual causes in one shot:

```bash
pcsync doctor -config ./config.json
```

```
✓ configuration    server https://cloud.example.ts.net · user vivian · root /home/vivian/PrivateCloud
✓ state database   /home/vivian/PrivateCloud/.pcsync/state.db
✓ server reachable https://cloud.example.ts.net (HTTP 200)
✓ sign in          credential accepted; tree root reachable
✓ client version   pcsync 1.0.0 matches the server
```

The version line is advisory: a client behind the server is a `!` warning, never a
`✗` — a version skew invites an update but does not stop a sync, so the preflight
still passes. A `dev` build is not compared to a release tag at all.

It separates "server unreachable" from "credential rejected" so you fix the right
thing — a `✗ sign in` with a `✓ server reachable` means the username or app
password is wrong, not the network.

The app password is exchanged for a short-lived **device token** on first
contact; the password itself never touches the file endpoints. A device token
can read and write your files but cannot manage account credentials — it cannot
mint another app password, the same limit the app password itself has.

## Building for other machines

The client is **pure Go — no CGO** — so it cross-compiles to every desktop from
one machine with no per-OS toolchain. That is the whole reason shipping a desktop
client is tractable here:

```bash
./build-release.sh            # linux, macOS and Windows binaries into dist/
VERSION=1.2.0 ./build-release.sh
```

Each build is static and stamped with its version; `dist/SHA256SUMS` lets a
download be verified — the client that checks every synced chunk should not ask
you to trust an unverified copy of itself. Check which version you're running,
and whether it matches the server:

```bash
pcsync version                        # the client build
pcsync version -config ./config.json  # ...and the server's, flagging a mismatch
```

> Platform-native **installers** (a `.msi`, a `.pkg`, a Homebrew/Scoop manifest)
> and an in-place **auto-updater** build on top of these binaries and are the
> remaining native-client work — they need signing keys and per-OS packaging
> that live outside this repo.

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
pcsync watch  -config ./config.json     # a live status line that updates in place
pcsync sync   -config ./config.json     # reconcile now (works even while paused)
pcsync pause  -config ./config.json     # stop automatic syncing (e.g. on a hotspot)
pcsync resume -config ./config.json     # resume it
```

`watch` is the headless counterpart to a tray icon — one self-refreshing line:

```
✓  Up to date — 1284 items · last sync 8s ago
```

The same state and summary logic drives a desktop tray shell; the icon is a thin
adapter over it.

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

### Conflicts

When a file changed on both sides, nothing is lost or merged: the server's
version keeps the original name and your edit is set aside as a copy. List the
ones awaiting a decision, then dismiss the reminder once you've dealt with the
files:

```bash
pcsync conflicts       -config ./config.json   # list copies needing a decision
pcsync conflicts clear -config ./config.json   # dismiss the list (files untouched)
```

Clearing only dismisses the reminder — the conflict copies on disk, the durable
record, are never touched by it.

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

## Ignoring local junk (.pcsyncignore)

Selective sync declines whole *server* subtrees; a `.pcsyncignore` file in the
sync root declines *local* paths by pattern, so build output, editor scratch
files and OS metadata never sync in either direction. It is a pragmatic subset of
`.gitignore`:

```gitignore
# never sync these
*.tmp
.DS_Store
node_modules/      # trailing slash: a directory (and everything under it)
/build             # leading slash: anchored to the sync root
src/*.log          # a path glob, anchored to the root
```

- A pattern with no slash matches a file or folder of that name **at any depth**.
- A trailing slash matches a directory only, and its whole subtree is skipped.
- A pattern containing a slash is anchored to the root and matched against the
  whole path. `*`, `?` and `[…]` are wildcards; `**` is not special.

The file is re-read at the start of every reconcile, so edits take effect on the
next pass. `pcsync status` shows how many rules are active. An ignored path is
never uploaded, never downloaded, and — importantly — a path you ignore *after*
it was already synced is left alone on the server, never trashed.

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
