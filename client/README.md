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

Conflict handling is deliberately minimal in this slice: when a file changed on
both sides, the local edit is kept and uploaded as a new version, and the server
preserves the remote edit in its version history — nothing is lost. Slice 4
turns that into a visible *conflict copy*.

## Setup

1. On the server, mint an app password (web UI → app passwords, or
   `cloudctl user app-password <name>`). It looks like `pcap_<lookup>_<secret>`
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

The `.pcsync` directory under the root holds the state database and download temp
files and is never synced.
