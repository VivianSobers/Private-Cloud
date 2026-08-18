# Phase 3 — Sync Engine Design

**Status: ✅ complete — 4/4 slices, both sides of the API.** The daemon this phase
built is also what Phase 6 wraps in a control surface; nothing here is
outstanding. Marks: ✅ done · 🟠 partial · ❌ not built; the whole-project ledger
is [status.md](status.md).

Written before any code, the same discipline
that carried Phases 1 and 2: make the expensive decisions deliberately, and keep
the document as the record of *why*. Where the code later diverges, the section
says so inline rather than being retconned.

**Exit criterion:** a folder on your laptop and the same folder on your desktop
hold the same files without you thinking about it. Edit on one, it appears on the
other within seconds. Edit the same file on both while offline, and reconnecting
produces a *conflict copy* — never a silent overwrite, never a lost edit. A 4 GB
file that changed by one block transfers one block, not four gigabytes.

That last clause is the whole reason this phase sits after Phase 2: the sync
engine is the client that finally cashes in content-addressed storage. Without
chunking, "sync" is just repeated whole-file upload; with it, sync is a diff.

---

## 0. What gates the start

Phase 2 is complete and verified: chunking, dedup, versioning and share links all
land green. Two things must be true before a sync client writes:

- ✅ [x] The CAS delta primitives exist and are reviewed (slice 2). A client that
      uploads chunks by hash is a new write path into the blob store, and it must
      verify content against its address before trusting a byte — an unverified
      chunk-upload endpoint is how one client corrupts everyone's dedup.
- ❌ [ ] A backup is current. Sync multiplies write volume and introduces deletes
      that propagate across devices; the first time a bug deletes the wrong file,
      it deletes it *everywhere*. The restore path is the safety net under that.

---

## 1. Scope

### In

| | |
|---|---|
| **Change journal** | A per-user, monotonic log of tree changes, with a stable cursor a client resumes from |
| **Delta protocol** | Fetch a file's manifest, ask which chunks the server already has, upload only the new ones, commit a manifest — block-level sync in both directions |
| **Go sync client** | A daemon: local state DB, initial sync, filesystem watching, apply-remote and push-local reconciliation |
| **Conflict resolution** | Base-version tracking, conflict detection, conflict copies, never data loss |

### Explicitly out

Real-time collaborative editing, selective/partial sync of subtrees (a later
refinement), LAN peer-to-peer transfer, mobile clients, and any GUI. The daemon
is a headless agent configured by a file; a tray app is Phase 4+ polish.

---

## 2. The shape

```
server tree  ──(change journal)──►  client         push:  client chunks locally,
     ▲                                  │                  asks "have?", uploads new,
     └──────────(delta upload)──────────┘                  commits a manifest
```

The server stays authoritative. A client never talks to another client; it
reconciles its local folder against the server, and the server's journal is the
single ordering of truth. Two clients converge because they both converge on the
server, not on each other — which is what keeps the protocol a two-party problem
instead of an n-party consensus one.

---

## 3. The change journal (slice 1)

A client that has been offline asks one question: *what changed since I last
looked?* The journal answers it with a cursor.

- Every sync-relevant change to a node — created, new version, moved, renamed,
  trashed, restored, purged — appends a row: `(owner, seq, node_id, kind)` where
  `kind` is `upsert` (the node is live at its current state) or `delete` (it is
  gone from the live tree). The row is minimal on purpose: it is an *invalidation*,
  not a snapshot. The client re-fetches the node's current state, so a change that
  is immediately superseded is self-healing rather than stale.
- `seq` is a **per-owner counter**, not a global `bigserial`. A bigserial assigns
  numbers at insert time, so a transaction holding seq 9 can commit *after* one
  holding seq 10 — and a reader that advanced its cursor to 10 would never see 9.
  A counter bumped inside the writing transaction (an `UPDATE ... RETURNING` on a
  per-owner `sync_state` row) serialises assignment behind that row's lock, so
  `seq` order equals commit order and a client that sees `seq` N is guaranteed
  every lower `seq` is already visible. The cost is that one user's concurrent
  writes serialise on their own counter — negligible for a personal cloud, and
  the correct trade for a cursor that cannot skip.
- Population is a **trigger** on `nodes`, for the same reason refcounts are: moves
  rewrite descendant paths in one statement and cascades delete rows no Go code
  names, so service-layer journaling would drift the first time a new write path
  appeared. The trigger fires only when `path`, `head_version_id` or `trashed_at`
  actually change — an `updated_at`-only touch is not a sync event.
- `GET /api/v1/changes?since=N` returns the owner's rows past `N`, each with the
  node's current state embedded for `upsert`s, plus `latest` (the head cursor) and
  `reset` (the client's cursor predates retained history, or the server was
  restored behind it — do a full re-sync). Retention prunes the journal's tail in
  GC; a client offline longer than the window re-syncs from scratch, which `reset`
  is what tells it to do.

---

## 4. The delta protocol (slice 2)

Both sides speak chunks, so a change transfers a diff.

- **Download:** the client fetches a file's manifest — the ordered list of chunk
  hashes and offsets — diffs it against the chunks it already holds locally, and
  pulls only the missing ones. Reassembly is the client's, byte-verified against
  each chunk's address exactly as the server verifies on read.
- **Upload:** the client chunks the local file with the *same* FastCDC + BLAKE3
  parameters (they are protocol, now, not an implementation detail), asks the
  server which of those hashes it already has, uploads only the new chunks, then
  commits a manifest referencing all of them.
- **The dangerous part** is the chunk-upload endpoint: it is the first path where
  a client writes raw addressed content. The server MUST recompute BLAKE3 over the
  received bytes and reject any mismatch — a chunk stored under the wrong address
  corrupts every file that later dedups against it, across users. "The client said
  so" is never sufficient for content addressing.
- Quota is charged on the committed manifest's logical size, as ever; a client
  cannot inflate its allowance by uploading chunks it never commits (those are
  unreferenced and GC reclaims them).

---

**Slice 2 delta-protocol notes, recorded where the next reader will look:**

- `PUT /chunks/{hash}` is the one endpoint that must never trust its input: the
  server recomputes BLAKE3 over the received bytes and returns 400 on any
  mismatch (`cas.PutChunk` / `TestPutChunkVerifiesAddress`). A chunk stored under
  the wrong address would corrupt every file that later dedups against it, across
  users — so the client's claim is checked, not believed.
- `GET /chunks/{hash}` is scoped by `UserReferencesChunk`: a chunk is global, but
  a user may read one only if they already hold a live file made of it, and a
  user who does not gets 404 — indistinguishable from the chunk not existing, so
  it is not an existence oracle for other users' content. `have` deliberately is
  a weaker signal (it answers "does anyone hold this exact content"), the
  unavoidable side-channel of cross-user dedup.
- The server owns the geometry. `CommitManifest` computes offsets and total size
  from the chunks' own recorded sizes, not the client's claim; the client
  supplies only the order and the whole-file hash. Quota is charged at commit on
  the manifest's logical size, so chunks uploaded but never committed cost the
  uploader nothing and are reclaimed by GC.
- Both formats still coexist: `manifest` reports `kind: "whole"` for a small
  blob, telling the client to fetch it in one piece rather than diff it.

## 5. The client (slice 3)

A headless daemon, one synced root, configured by a file.

- A **local state database** (SQLite) records, per path, the version identity it
  last synced, the file's chunk list, size and mtime. This is the base against
  which both local edits and remote changes are judged — never the filesystem
  mtime alone, which lies across restores and clock changes.
- **Initial sync** walks the server tree, materialises it locally, and records the
  journal head as the starting cursor.
- **Steady state** is two loops: apply remote changes pulled from the journal, and
  push local changes detected by watching the filesystem (fsnotify) and by a
  periodic rescan that catches what the watcher missed. Both reconcile through the
  local state DB, so an edit already applied is a no-op rather than a fight.

---

**Slice 3 client notes, recorded where the next reader will look:**

- The client is a **separate Go module** (`client/`), not a package under
  `server/`. It ships to laptops, so it must build with no CGO and must not drag
  in pgx, the blob store or WebAuthn. It cannot import the server's `internal`
  packages across the module boundary, which is the right boundary: the FastCDC +
  BLAKE3 parameters are re-declared in `client/internal/chunk` to match the
  server, because a protocol is a contract, not shared code.
- A headless client cannot run a WebAuthn ceremony, and `/api/v1/*` takes a
  session token, not Basic auth — deliberately, so no route inherits Basic auth
  by accident. So there is exactly one endpoint that bridges the two:
  `POST /api/v1/auth/token` exchanges an app password (Basic) for a device bearer
  token (`SessionKindDevice`). `requireAuth` confines a device session away from
  credential management, because an app password cannot mint another credential
  and a token exchanged from one must not either.
- Two hashes per file in the local state DB, because the two sides address content
  differently: the client's own whole-file BLAKE3 (the baseline a local edit is
  judged against) and the server's reported hash — blake3 for chunked, sha256 for
  a small whole-file blob (the baseline a remote change is judged against). Local
  change detection gates on size+mtime and only re-hashes when they move, so an
  untouched tree costs a stat per file, not a read.
- Pull before push each pass. Applying the server first means the local scan
  pushes against a fresh baseline. Where both sides moved, the pull declines to
  overwrite the local edit and the push uploads it as a new version — the server
  keeps the remote edit in history (Phase 2 versioning), so slice 3 already loses
  no data; slice 4 makes the conflict visible rather than history-only.

## 6. Conflict resolution (slice 4)

The one thing the sync engine must never do is lose an edit.

- A conflict is detected by **lineage, not clocks**: the client synced a file at
  base version B. If the server's head has moved past B *and* the local file has
  changed since B, both sides edited independently — a conflict.
- The resolution is a **conflict copy**: the local edit is preserved as
  `name (conflict from HOST, DATE).ext` and uploaded as its own file, while the
  server's version takes the original name. Nothing is overwritten; the user
  resolves by choosing, which is a decision they can make and a merge is not.
- Deletes conflict too: a file edited on one device and deleted on another
  resurfaces as a conflict copy rather than honouring the delete, because a delete
  that destroys an unseen edit is the same data loss by another name.

**Slice 4 conflict notes, recorded where the next reader will look:**

- Detection is lineage, implemented as two comparisons against the recorded base
  (`state.Entry`): the server's content hash no longer equals `RemoteHash` (its
  head moved) *and* the local file's recomputed BLAKE3 no longer equals `Hash`
  (the bytes moved). Both true is a conflict; neither timestamp is consulted, so
  clock skew between two machines can neither fabricate nor mask one.
- The resolution never overwrites and never merges. `conflictCopy` renames the
  local edit to a free `name (conflict from HOST DATE).ext`, drops the original
  path's state so the freed name can take the server's version on the next
  `pullDown`, and leaves the renamed file stateless so the push uploads it as a
  new file. Both files then exist on the server and on every device; the user
  resolves by choosing, a decision a person can make and a merge cannot.
- The same path handles a delete seen through the journal, a delete found during a
  full reconcile (`removeVanished`), and a both-sides edit — one function, so the
  three routes to the same hazard cannot drift apart.

---

## 7. Slices

Same discipline as before: each slice ends green, committed, and useful.

| Slice | Contents | Status |
|---|---|---|
| **1** | Change journal: `sync_state` counter, `changes` table + trigger, `GET /changes` with cursor/reset, retention in GC | ✅ per-owner counter gives gap-free commit-order seqs; trigger records upsert/delete on path/head/trash changes; endpoint embeds node state and signals `latest`/`reset`/`has_more`; GC prunes the tail |
| **2** | Delta protocol: manifest fetch, chunk `have` query, verified chunk upload, manifest commit | ✅ `GET /nodes/{id}/manifest`, `POST /chunks/have`, `PUT /chunks/{hash}` (BLAKE3-verified), `GET /chunks/{hash}` (scoped to referencing users), `POST /manifests`; server owns offsets and compression, quota charged at commit |
| **3** | Go sync client: local state DB, initial sync, fsnotify + rescan, apply/push loops | ✅ a separate pure-Go module (`client/`, `pcsync`): SQLite state DB keyed by path, initial tree reconcile, incremental journal replay, delta upload/verified download, fsnotify + poll + rescan loops; app password exchanged for a confined device token |
| **4** | Conflict resolution: base-version tracking, lineage detection, conflict copies | ✅ detection by lineage against the recorded base (RemoteHash moved *and* local Hash moved), never by clocks; the local edit is set aside as `name (conflict from HOST DATE).ext` and pushed as its own file while the server's version keeps the name; delete-vs-edit resurfaces the edit the same way rather than honouring the delete |

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| A missed change diverges two devices permanently | Journal populated by a trigger inside the writing transaction; the per-owner counter makes the cursor gap-free so no change is skipped |
| A client uploads garbage under a valid chunk hash | The server recomputes BLAKE3 and rejects any address mismatch before the byte is trusted |
| A propagated delete destroys an unseen edit | Deletes conflict against local edits and resurface as conflict copies; never a silent removal |
| Conflict detection fooled by clock skew | Detection is by version lineage, never by comparing timestamps |
| Journal grows without bound | Retention prunes the tail in GC; `reset` tells a too-old client to full-resync |
| The watcher misses a filesystem event | A periodic rescan reconciles against real file hashes, so a missed inotify event is caught, not lost |
