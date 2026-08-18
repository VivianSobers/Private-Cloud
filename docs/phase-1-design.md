# Phase 1 — MVP Design

**Status: ✅ complete — 7/7 slices, both sides of the API.** The only unticked
items in this document are the three `sudo` gates in §0, which are the operator's
and cannot be verified from a checkout. Marks: ✅ done · 🟠 partial · ❌ not built;
the whole-project ledger is [status.md](status.md).

All seven slices are implemented and committed; see the
slice table in §7. This document was written before any code so the expensive
decisions got made deliberately, and it is kept as the record of *why* the
shipped design looks the way it does. Where the code diverged from the plan the
section says so inline — the plan is not retconned to match.

**Exit criterion:** you stop using Google Drive for manual file workflows. Not
"the API works" — you actually keep real files here, on purpose, because it's
the more convenient option.

---

## 0. What gates the start

Phase 0 is up but not fully verified. Not all of it blocks Phase 1 — separate
the two:

**Must close before Phase 1 writes real data:**

- ❌ [ ] `sudo ./scripts/restore-test.sh` passes — the exit gate
- ❌ [ ] Snapshot ladder confirmed filling in (`zfs list -t snapshot | wc -l` grows)
- ❌ [ ] One restic backup completed and restored successfully

**Can run in parallel with Phase 1:** Grafana dashboards, ntfy alert chain,
image digest pinning, textfile collectors, production hardware.

The reasoning: Phase 1 is the first phase that creates data you'd be upset to
lose. Monitoring gaps cost you visibility; storage gaps cost you the files.
Nothing else in Phase 0 is load-bearing for writing Go.

> **Still open.** The three boxes above are deliberately unticked: each needs
> `sudo` on the real server and none of them can be verified from a dev
> checkout. The code is built and tested, so the gate now reads "before you
> put files in here you'd cry about losing", not "before you write Go".
> Run `sudo ./scripts/restore-test.sh` and tick them yourself.

---

## 1. Scope

### In

| | |
|---|---|
| **Auth** | Passkeys (WebAuthn) + recovery codes, sessions, admin bootstrap |
| **Files** | Folder tree, upload, download with Range, rename, move, trash |
| **Uploads** | Resumable (TUS) — retrofitting resumability later is painful |
| **Web UI** | Browse, upload, download, preview, trash |
| **WebDAV** | Read/write endpoint over the same storage layer |
| **Search** | Filename + path only, Postgres trigram |

### Explicitly out

Content-addressed storage, chunking, dedup, version history UI, share links,
sync engine, thumbnails, OCR/ML, multi-user invitations, quotas enforcement.

Every one of these has a home in Phase 2–4. The discipline that matters: **Phase
1 stores whole files behind an interface**, so Phase 2 replaces the
implementation without touching the API or the schema's shape.

---

## 2. The one decision that determines whether Phase 2 hurts

Phase 2 replaces whole-file storage with content-addressed chunks. If Phase 1's
schema assumes "a file has bytes," that migration is a rewrite. If it assumes
"a file has an ordered history of immutable versions, each pointing at storage,"
the migration is swapping what `file_versions` points at.

**So Phase 1 builds the versioned schema and only ever writes one version per
file.** No version history UI, no version API — just the table shape, correct
from the start. This costs maybe a day now and saves a schema migration over
live data later.

```
Phase 1:  node ──► file_version ──► blob (whole file, one row)
Phase 2:  node ──► file_version ──► manifest ──► chunks[] (CAS)
                   ^^^^^^^^^^^^ unchanged
```

Same for the `BlobStore` interface: `Put/Get/Has/Delete` by key. Phase 1 backs
it with "one file per blob on `tank/blobs`". Phase 2 swaps in FastCDC+BLAKE3
behind the same four methods.

---

## 3. Schema sketch

Not final DDL — the shape, and the decisions embedded in it.

```sql
-- ---- identity ----
users               (id, username, display_name, is_admin, quota_bytes,
                     disabled_at, created_at)
webauthn_credentials(id, user_id, credential_id UNIQUE, public_key, sign_count,
                     aaguid, name, created_at, last_used_at)
recovery_codes      (id, user_id, code_hash, used_at)
sessions            (id, user_id, token_hash, kind, user_agent,
                     created_at, last_seen_at, expires_at, revoked_at)

-- ---- the tree ----
nodes               (id, owner_id, parent_id, name, name_fold, kind,
                     path, head_version_id, trashed_at, created_at, updated_at)
file_versions       (id, node_id, size, mime, blob_id, created_at, created_by)
blobs               (id, storage_key, size, sha256, refcount, created_at)

-- ---- uploads in flight ----
uploads             (id, user_id, parent_id, name, size, offset,
                     tus_id, expires_at)
```

**Three details worth arguing about now rather than discovering later:**

**`name_fold`.** Linux filesystems are case-sensitive; macOS and Windows are
not. A WebDAV client on a Mac will happily try to create `Photos` next to
`photos` and then behave unpredictably. Store the display name *and* a folded
(lowercased, Unicode-normalized) form, with `UNIQUE (parent_id, name_fold)
WHERE trashed_at IS NULL`. Retrofitting this after clients exist means
reconciling real collisions.

**`path` alongside `parent_id`.** Adjacency list is the truth; materialized path
is a denormalized cache for prefix queries (`WHERE path LIKE '/photos/%'`) and
for the inherited-ACL model in Phase 2. Cost: renaming a folder rewrites the
subtree's paths. At personal scale that's a bounded `UPDATE`, and it buys cheap
subtree reads forever.

**Write order: blob first, then the DB row.** If the process dies between them,
you get an orphaned blob (harmless, GC-able) rather than a DB row pointing at
bytes that don't exist (corruption the user sees). Never the reverse. Phase 1
needs a `fsck`-style consistency checker from the start — an hour of work that
tells you whether the invariant actually holds.

---

## 4. Auth, and the way this bites you

Passkeys are the right primary factor: phishing-resistant, no password database
to breach. But there's a failure mode worth being blunt about.

**If passkeys are the only credential and you lose the authenticator, you are
locked out of your own file server with no reset path.** There is no "forgot
password" email, because there's no email service and no second admin.

So Phase 1 ships, non-negotiably:

1. **Recovery codes** generated at registration, shown once, stored hashed
   (argon2id). Print them. Same envelope as the ZFS passphrase.
2. **A CLI escape hatch** — `cloudctl user reset-auth <username>`, runnable
   over SSH on the server, that clears credentials so you can re-register.
   Root on the box is already total access; this doesn't weaken the model.

Session model: opaque token, `HttpOnly; Secure; SameSite=Lax`, server-side
session row so revocation is immediate. Device tokens for WebDAV/CLI clients get
their own row and appear in the UI as revocable entries.

WebDAV can't do WebAuthn — it needs Basic auth. That means **per-device app
passwords**, scoped and revocable, never the account credential. Worth designing
in from the start rather than bolting on when the first WebDAV client fails.

---

## 5. Repo layout

```
server/
  cmd/api/            main, config, wiring
  cmd/cloudctl/       admin CLI (user reset-auth, fsck, gc)
  internal/
    auth/             webauthn, sessions, recovery codes
    files/            tree operations, trash
    blob/             BlobStore interface + filesystem impl
    upload/           TUS handler
    webdav/           x/net/webdav FileSystem over files/
    search/           trigram queries
    httpapi/          handlers, middleware, error mapping
  migrations/         goose SQL migrations
web/                  React + TS + Vite
api/                  OpenAPI spec (generated from handlers)
```

Module boundaries are enforced by keeping interfaces narrow — `files` depends on
`blob.Store`, never on the filesystem. That's what makes Phase 2's swap cheap and
what would let a module become a service if there's ever a second machine.

---

## 6. Stack, with two revisions to the original design

| | Choice | Note |
|---|---|---|
| Router | **stdlib `net/http`** | Go 1.22+ has method+wildcard patterns. **Revising the original chi recommendation** — chi's remaining edge is thin and stdlib is one less dependency in the auth path. |
| Queries | sqlc | Type-safe, no ORM magic |
| Migrations | goose | Plain SQL, readable in review |
| Jobs | **none yet** | **Revising: don't add River in Phase 1.** There are no background jobs until thumbnails and indexing in Phase 2. Adding a queue before there's a job is infrastructure for its own sake. |
| Uploads | tusd v2 as a library | Resumability is the hard part; don't hand-roll |
| WebAuthn | go-webauthn | Never hand-roll auth crypto |
| Frontend | React + TS + Vite, TanStack Query, Uppy | Uppy speaks TUS natively |

**Instrument from the first commit.** Phase 0 already runs Prometheus and
Grafana — a `/metrics` endpoint on the API from day one means you watch the
thing take shape instead of retrofitting observability. This is the payoff for
doing Phase 0 first, and it's easy to forget to collect.

---

## 7. Build order

Vertical slices, each ending somewhere usable. All seven shipped.

| # | Slice | Done when | Shipped as |
|---|---|---|---|
| 1 | **Skeleton** — config, DB, migrations, `/healthz`, `/metrics`, structured logs, compose service behind Caddy | Grafana shows API metrics | ✅ `internal/{config,db,metrics}` |
| 2 | **Auth** — passkey register/login, sessions, recovery codes, admin bootstrap, `cloudctl` | You log in with a passkey from your phone | ✅ `internal/auth`, `cmd/cloudctl` |
| 3 | **Files core** — tree CRUD, simple upload, download w/ Range, trash, `fsck` | `curl` round-trips a file | ✅ `internal/files` |
| 4 | **Resumable upload** — TUS | A 2 GB upload survives killing the connection | ✅ `internal/files/uploads.go` |
| 5 | **Web UI** — browse, upload, download, preview, trash | You use it in a browser without `curl` | ✅ `web/` |
| 6 | **WebDAV** | Your OS file manager mounts it | ✅ `internal/webdavfs` |
| 7 | **Search** — trigram on name/path | Finding a file is faster than remembering where it is | ✅ `internal/files/search.go` |

Two things came out differently from the plan and are worth recording:

- **App passwords, not in the plan at all.** WebDAV cannot do WebAuthn — there
  is no ceremony a Finder mount can drive — so slice 6 needed a second
  credential type (`internal/auth/apppassword.go`). Scoped, revocable, and
  never usable against the JSON API.
- **Search ranks, it does not just filter.** The plan said "trigram on
  name/path". A bare `LIKE` over a trigram index returns matches in table order,
  which is useless once you have more than a screenful. The shipped query ranks
  exact → prefix → similarity → recency.

**Auth is slice 2, before files, deliberately.** Retrofitting authorization into
handlers that assumed a single user is miserable and error-prone. Every file
handler should be written against a real `user_id` from the start.

**WebDAV at 6, before the sync engine exists**, is the highest-leverage item in
the phase: it makes every OS file manager and every WebDAV-speaking app a
working client months before Phase 3.

---

## 8. Risks

| Risk | Mitigation |
|---|---|
| Passkey lockout | Recovery codes + `cloudctl reset-auth`, both tested before real data |
| DB/disk drift | Blob-before-row write ordering; `fsck` from slice 3 |
| Case-collision from WebDAV clients | `name_fold` unique constraint from the first migration |
| Phase 2 CAS migration is painful | Versioned schema + `BlobStore` interface from day one (§2) |
| Scope creep into Phase 2 | The "explicitly out" list in §1 is the contract |
| Building on unverified storage | The §0 gates |

---

## 9. Open questions — resolved

1. **Single-user or multi-user tables from the start?** ✅ Multi-user schema,
   single-user UI, as recommended. Every query carries `owner_id`; it is a
   field on the WebDAV `FileSystem` rather than a parameter, so there is no
   handler that could forget to pass it.
2. **Does the dev box stay the dev box?** ✅ Started clean. Phase 1 data was
   disposable, so nothing had to be migrated.
3. **Web UI: how much polish in Phase 1?** ✅ Unstyled-but-usable — one
   hand-written `styles.css`, no framework, no router.
4. **Preview scope.** ✅ Serve the original, capped by size and MIME type; no
   thumbnailing. Still the right answer until Phase 2 adds derived renditions.

---

## 10. Rough sizing

Slices 1–3 are the substance; 4–7 are each self-contained. Expect the auth slice
to take longer than it looks (WebAuthn ceremonies have real edge cases across
platforms) and WebDAV to be faster than it looks (`x/net/webdav` does the
protocol; you implement a `FileSystem` interface).

The honest risk to the timeline isn't any single slice — it's slice 5 expanding
indefinitely, because a file browser UI is infinitely polishable. Timebox it.
