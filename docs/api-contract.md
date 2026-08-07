# API contract — the seam between the two tracks

This is the **one file both developers edit** (see
[roadmap-split.md](roadmap-split.md)). It is the source of truth for the
`/api/v1` surface the clients code against. Rules:

- **Additive only.** New endpoints and fields are fine; changing or removing the
  shape of an endpoint a client already uses is not.
- **Contract-first.** A new feature's endpoints land here *before* the handler
  or the UI is built. That is what lets the front-of-API track start immediately
  against a mock.
- Keep this human-readable; a generated OpenAPI (`openapi.yaml`) can be derived
  from the handlers later and checked against this doc by the contract test.

---

## Conventions

- Base path `/api/v1`. JSON in/out. Auth is cookie **or** `Authorization:
  Bearer` (device token); Basic is accepted **only** at `POST /auth/token`.
- Timestamps are RFC 3339 UTC. IDs are UUIDs unless noted.

### Errors

Every failure, from every endpoint, has one shape — a **nested** object, not a
flat one:

```json
{
  "error": {
    "code": "not_found",
    "message": "no such file or folder",
    "request_id": "4f9c1e2a8b7d0c3e5a6f9b12"
  }
}
```

`code` is stable and safe to branch on; `message` is human text and may be
reworded. `request_id` is present on most responses and is echoed in the
`X-Request-Id` header — quoting it is what makes a user's "it broke" answerable
from the server log without asking them to reproduce it.

`Content-Type: application/json; charset=utf-8` and `Cache-Control: no-store` on
all JSON responses.

> Corrected 2026-08-07. This section previously documented
> `{"error": "<code>", "message": "<text>"}` — flat, and with no `request_id`.
> The server has never produced that shape (see `errorBody` in
> `server/internal/httpapi/server.go`). A client coded against the old text would
> read `error` as a string and get an object, so every error path would fail to
> parse. Recorded rather than quietly edited because the whole point of this file
> is that the other track can trust it without reading the handlers.

## Shipped surface (Phases 1–4) — do not break

Enumerated from the registered routes in `httpapi.Server.Handler`. Every route
below exists today. Unless marked otherwise a route requires authentication and
returns `200`.

### Shared object: `node`

Returned wherever a file or folder appears. Absent fields are omitted, not null.

```json
{
  "id": "uuid", "kind": "file|folder", "name": "report.pdf",
  "path": "/work/report.pdf", "parent_id": "uuid",
  "created_at": "2026-08-07T09:00:00Z", "updated_at": "2026-08-07T09:00:00Z",
  "size": 12345, "mime": "application/pdf",
  "sha256": "hex", "blake3": "hex",
  "trashed_at": "2026-08-07T09:00:00Z"
}
```

- `size`/`mime` only on `kind: "file"`.
- **`sha256` XOR `blake3`** — the key names the algorithm, because whole-file
  blobs are SHA-256 and chunked (manifest-backed) files are BLAKE3-256. A client
  verifying a download must run the one it was given. Never both.
- `parent_id` is absent on the root; `trashed_at` only on trashed nodes.
- Storage detail (blob keys, internal version ids) is deliberately **not** here.

### Unauthenticated / operational

| Route | Notes |
|---|---|
| `GET /healthz` | liveness only, never touches the DB |
| `GET /readyz` | `503` when the database is unreachable |
| `GET /metrics` | Prometheus exposition |
| `GET /api/v1/version` | `{service, version, commit}` |

### Auth

| Route | Notes |
|---|---|
| `GET /auth/status` | `{bootstrap_required, user_count, oidc_enabled}` — unauthenticated |
| `POST /auth/register/begin`, `/finish` | WebAuthn enrolment ceremony |
| `POST /auth/login/begin`, `/finish` | WebAuthn assertion ceremony |
| `POST /auth/recovery/redeem` | `{username, code}` → session + `next_step` |
| `POST /auth/recovery/regenerate` | new codes; invalidates the previous set |
| `POST /auth/logout` | clears the session cookie |
| `GET /auth/oidc/login` → `302` | starts SSO; `404 oidc_disabled` when unconfigured |
| `GET /auth/oidc/callback` → `302` | to `/` on success, `/?sso_error=1` on any failure |
| `POST /auth/token` | **Basic** app-password → device bearer token |
| `GET /auth/me` | `{user, session_kind, remaining_recovery_codes}` |
| `GET`/`DELETE /auth/credentials[/{id}]` | passkeys |
| `GET`/`DELETE /auth/sessions[/{id}]` | active sessions |
| `GET`/`POST`/`DELETE /auth/app-passwords[/{id}]` | plaintext returned once, at creation |

The auth routes and the public share routes are rate limited; exceeding it is
`429 rate_limited` with `Retry-After`.

### Files & folders

| Route | Notes |
|---|---|
| `GET /nodes/root` | `{node}` |
| `GET /nodes/{id}` | `{node, tags}` — `{id}` accepts the literal `root` |
| `GET /nodes/{id}/children` | `{parent, children}` |
| `GET /nodes/resolve?path=/a/b` | address by path instead of id |
| `POST /folders` | `{parent_id, name}` → `201 {node}` |
| `PATCH /nodes/{id}` | `{name?, parent_id?}` — rename and move are one atomic call |
| `DELETE /nodes/{id}` | → trash; `{status, nodes_affected}` |
| `POST /upload?parent_id=&name=` | raw body or multipart → `201 {node}` |
| `GET`/`HEAD /nodes/{id}/content` | ranges, ETag, `?download=1` for attachment |
| `GET /usage` | `{live_bytes, trash_bytes, total_bytes, file_count, quota_bytes?, available_bytes?}` |
| `GET /trash`, `DELETE /trash` | list / empty |
| `POST /trash/{id}/restore`, `DELETE /trash/{id}` | restore / purge permanently |
| `POST /admin/fsck?repair=` | **admin only** |

### Versions

`GET /nodes/{id}/versions` · `POST /nodes/{id}/versions/{versionId}/restore` ·
`GET`/`HEAD /nodes/{id}/versions/{versionId}/content`

### Resumable uploads (tus 1.0.0)

`OPTIONS /uploads` (**unauthenticated** — advertises protocol support before a
client has anywhere to put credentials) · `POST /uploads` · `HEAD`/`PATCH`/
`DELETE /uploads/{id}`

### Sync

`GET /changes` (cursor journal; signals `reset` when the cursor is older than
the retained tail) · `GET /nodes/{id}/manifest` · `POST /chunks/have` ·
`GET`/`PUT /chunks/{hash}` · `POST /manifests`

### Search

`GET /search?q=&kind=&under=&include_trashed=&limit=&offset=&semantic=`

- Minimum query length **2**.
- `limit` is clamped server-side to 200; `has_more` reflects the clamped limit.
- Results are `node` plus `matched_path`, `matched_content`, and — in semantic
  mode — `semantic: true` and `score` (cosine, 0–1).
- `semantic=true` needs the inference sidecar. Without it: `503
  semantic_unavailable`, which clients should treat as "retry lexically **and
  say so**", not as an error.

### Tags

`GET`/`POST /nodes/{id}/tags` · `DELETE /nodes/{id}/tags/{tag}` ·
`GET /tags` (with counts) · `GET /tags/{tag}?limit=&offset=`

Tags are lowercased and trimmed server-side, max 64 chars, no control
characters (`400 invalid_tag`). `source` is `auto` (worker-derived, replaced on
re-extraction) or `user` (explicit, never clobbered).

### Shares

`POST /nodes/{id}/shares` · `GET /shares` · `DELETE /shares/{id}`

Public plane, **unauthenticated** and rate limited: `GET /s/{token}` ·
`POST /s/{token}/unlock` · `GET`/`HEAD /s/{token}/content`. The token is
returned once, at creation, and never stored.

### WebDAV

Mounted at `/dav`, outside `/api` — a different protocol with a different auth
scheme (Basic, using an app password).

## Proposed surface (Phase 5+) — additive, land here first

Each new phase from the split adds its endpoints below **before** implementation.

### Phase 5 — Photos & media

**Media metadata.** Extracted by a new `media` job kind, alongside the existing
`extract`. Content-addressed like `doc_text`, so identical images are read once.
It appears as an optional `media` object on a `node`, absent until the job runs:

```json
"media": {
  "width": 4032, "height": 3024, "orientation": 1,
  "taken_at": "2026-07-14T18:22:05Z",
  "camera": "Pixel 8 Pro",
  "gps": { "lat": 51.5072, "lon": -0.1276 },
  "duration_ms": 12500,
  "variants": ["thumb", "preview"]
}
```

- Every field optional — a PNG has no `taken_at`, a video has no `gps`.
- `taken_at` is **not** `created_at`: it is when the shutter fired, and it is
  what a timeline sorts by. Falls back to `updated_at` when absent, which the
  client must do itself so the fallback stays visible.
- `gps` is omitted entirely if absent. Note it is exact; there is no blurring.
- `variants` lists which derived sizes exist **now**, so a gallery knows whether
  to request a thumbnail or fall back to the original rather than guessing and
  getting a 404 per tile.

**Variant retrieval.** A parameter on the existing content route, not a new one —
so range requests, ETags, `Cache-Control` and the share plane all keep working
unchanged:

```
GET /nodes/{id}/content?variant=thumb|preview|original     (default: original)
```

- `thumb` ≈ 320px longest edge, `preview` ≈ 1600px. Both preserve aspect ratio.
- `404 variant_unavailable` when the media job has not produced it yet — an
  honest miss the client can retry, not a silent fallback to a 12 MB original
  that would make a gallery of 200 tiles pull gigabytes.
- Variants are stored in CAS and reference-counted like any other content, so
  they are garbage-collected with the file.

**Albums.** A user-ordered collection of nodes. An album is *not* a folder: a
node can be in many albums, and being in one does not move it.

| Route | Body / result |
|---|---|
| `GET /albums` | `{albums: [album]}` |
| `POST /albums` | `{name, description?}` → `201 {album}` |
| `GET /albums/{id}` | `{album, items: [node]}` — paged with `limit`/`offset` |
| `PATCH /albums/{id}` | `{name?, description?, cover_node_id?}` |
| `DELETE /albums/{id}` | deletes the album only, **never** the files in it |
| `POST /albums/{id}/items` | `{node_ids: [uuid], position?}` → `201` |
| `DELETE /albums/{id}/items/{nodeId}` | removes from the album |
| `PATCH /albums/{id}/items` | `{node_ids: [uuid]}` — full order, for drag-reorder |

```json
"album": {
  "id": "uuid", "name": "Iceland 2026", "description": "",
  "item_count": 214, "cover_node_id": "uuid",
  "created_at": "...", "updated_at": "..."
}
```

- Ordering is explicit and user-controlled; `PATCH .../items` replaces the whole
  order in one call, because a drag-reorder that issues N position updates is
  N chances to end up half-applied.
- Adding a node already in the album is a no-op, not a duplicate or an error, so
  a retried request is safe.
- Deleting an album never touches file content. Worth stating because it is the
  question every user has before they click it.

**Timeline.** The gallery's primary read, kept separate from `/search` because it
sorts by `taken_at` and pages by date rather than by relevance:

```
GET /media/timeline?from=&to=&limit=&offset=
  → {items: [node], has_more: bool}
```

### Phase 6 — Native clients

Almost no new server surface — the point of this phase is that the clients
consume the API that already exists. Two additions:

**Devices.** A device is a session of kind `device`, minted at
`POST /auth/token`. `GET /auth/sessions` already returns these, but a device list
wants the *client's* identity, not the session's.

| Route | Body / result |
|---|---|
| `GET /devices` | `{devices: [device]}` |
| `PATCH /devices/{id}` | `{name}` — let a user rename "unknown device" |
| `DELETE /devices/{id}` | revokes the token; the client gets `401` on its next call |

```json
"device": {
  "id": "uuid", "name": "guru-laptop", "platform": "linux",
  "app_version": "0.4.1", "last_seen_at": "...", "created_at": "...",
  "current": true
}
```

- `current` marks the caller's own device, so a UI can avoid offering "revoke"
  on the session doing the revoking without inferring it from ids.
- Revocation is immediate — the token is checked per request, not cached — which
  is the property that makes "I lost my laptop" a real answer.
- `platform` and `app_version` come from the client's `User-Agent` at token
  creation. Advisory only: they are self-reported and must never gate anything.

**Push (optional).** Deliberately a *hook*, not a service: this server does not
talk to APNs or FCM, and should not learn to.

```
POST /devices/{id}/push        {endpoint, keys}   register a Web Push subscription
DELETE /devices/{id}/push                         unregister
```

A client that does not register one simply polls `GET /changes`, which is the
existing, working path — push is a latency optimisation, never a correctness
requirement. Any client must work with it switched off.

### Phase 7 — Multi-user & sharing

> **The one phase with a compatibility hazard.** Everything today is
> owner-scoped: every query filters `owner_id = $me`, and search and tags are
> owner-global. Introducing "files I can see but do not own" widens what those
> endpoints return. That is *additive in shape* — no field changes — but it is a
> **semantic** change to existing endpoints, and it is the one place this
> phase can break a client that assumed everything it saw was its own.
>
> Rule adopted here: **shared content is excluded from existing endpoints
> unless the client opts in** with `?include_shared=true`. Default behaviour is
> unchanged, so an old client keeps seeing exactly what it sees today. The
> alternative — widening the default — would silently change the meaning of
> `GET /nodes/{id}/children` and `/search` for every already-shipped client.

**Grants.** A grant gives a user access to one node (and, for a folder,
everything beneath it).

| Route | Body / result |
|---|---|
| `GET /grants` | `{granted: [grant], received: [grant]}` — both directions |
| `POST /nodes/{id}/grants` | `{username, role}` → `201 {grant}` |
| `PATCH /grants/{id}` | `{role}` |
| `DELETE /grants/{id}` | revoke; effective immediately |

```json
"grant": {
  "id": "uuid", "node_id": "uuid", "path": "/work/shared",
  "owner": "guru", "grantee": "vivian",
  "role": "viewer|editor|owner",
  "inherited_from": "uuid",
  "created_at": "..."
}
```

- Three roles only. `viewer` reads, `editor` reads and writes, `owner` is the
  file's actual owner and cannot be granted away. Resist adding a fourth without
  a concrete need — a permission model people cannot hold in their heads is one
  they misconfigure.
- Grants are **per node and inherit down a folder**. `inherited_from` names the
  ancestor a grant came from, so a UI can explain *why* someone has access
  instead of showing an unexplained entry.
- A grant never moves or copies anything. The file stays in the owner's tree;
  the grantee reaches it at `GET /shared` or with `?include_shared=true`.
- Quota is charged to the **owner**, always. Otherwise sharing a folder would
  let one user spend another's quota.

**Reading shared content.**

```
GET /shared                          → {items: [node]} — roots granted to me
GET /nodes/{id}/children?include_shared=true
GET /search?include_shared=true
GET /tags?include_shared=true
```

When shared content is included, each `node` additionally carries:

```json
"access": { "role": "viewer", "owner": "guru", "shared": true }
```

`access` is absent on a node the caller owns — its absence means "mine", which
keeps the common response unchanged in both size and meaning.

**Search and tags under ACLs.** Semantic search ranks over embeddings that are
content-addressed and therefore shared between users by construction. The filter
must be applied to the **node** rows, never to the vectors, or one user's query
could surface the existence of another's document through a similarity score.
Tag counts at `GET /tags` are likewise per-caller, not per-tag-globally.

**Admin.** All `403` for non-admins.

| Route | Body / result |
|---|---|
| `GET /admin/users`, `POST /admin/users` | list / create |
| `PATCH /admin/users/{id}` | `{display_name?, is_admin?, disabled?, quota_bytes?}` |
| `DELETE /admin/users/{id}` | disables and revokes; does **not** delete content |
| `GET /admin/users/{id}/sessions`, `DELETE .../sessions/{sid}` | |
| `GET /admin/audit?actor=&action=&from=&to=&limit=&offset=` | append-only |

```json
"audit_entry": {
  "id": "uuid", "at": "...", "actor": "guru", "action": "grant.create",
  "target": "/work/shared", "request_id": "...", "detail": {}
}
```

The audit log records **authorisation-relevant** events — grants, role changes,
logins, admin actions, share creation — not every read. A log that records
everything is one nobody reads, and on this hardware it would outgrow the files
it describes. `request_id` ties an entry back to the API access log.

### Phase 8 — Advanced intelligence

Every endpoint here depends on a GPU sidecar and must degrade the way semantic
search already does: **`503` with a stable code, never a 500 and never a hang.**
The file API stays fully functional with all of this switched off — that is the
property the whole worker split exists to protect.

**People (face clustering).** A cluster is unnamed until a person names it; the
system never guesses an identity.

| Route | Body / result |
|---|---|
| `GET /people` | `{people: [person]}` |
| `GET /people/{id}` | `{person, items: [node]}` — paged |
| `PATCH /people/{id}` | `{name}` — name a cluster |
| `POST /people/{id}/merge` | `{into}` — two clusters are the same person |
| `DELETE /people/{id}` | forget the cluster; does not touch photos |
| `POST /nodes/{id}/faces/{faceId}/reassign` | `{person_id}` — fix one wrong face |

```json
"person": {
  "id": "uuid", "name": "Ada", "face_count": 87,
  "cover_node_id": "uuid", "cover_box": [0.41, 0.22, 0.12, 0.16]
}
```

- `cover_box` is `[x, y, w, h]` as fractions of the image, not pixels, so a
  client can crop from whichever variant it already has.
- Merge and reassign exist because clustering is *going to* be wrong, and a
  faces feature with no correction path is one people stop trusting after the
  first mistake.
- Face data is derived and per-owner. It is **not** content-addressed like
  embeddings: two users owning the same photo should not share a "people" graph.

**Similar files.**

```
GET /nodes/{id}/similar?limit=   → {results: [node + score]}
```

Reuses the existing embedding space for documents and an image-embedding variant
for photos; `503 semantic_unavailable` when no sidecar is configured, exactly as
`/search?semantic=true` does today.

**RAG chat.**

```
POST /chat   {question, scope?: {under?, tags?, node_ids?}, stream?: bool}
```

Non-streaming returns:

```json
{
  "answer": "…",
  "citations": [
    { "node_id": "uuid", "path": "/work/report.pdf", "chunk_seq": 3, "score": 0.82 }
  ],
  "model": "…"
}
```

With `stream: true`, `text/event-stream`: `token` events, then one `citations`
event, then `done`.

- **Citations are mandatory, not decorative.** An answer over someone's own
  documents that cannot say which document it came from is unverifiable, and a
  confident wrong answer about your own files is worse than no feature. Clients
  should render them next to the answer, not behind a disclosure.
- `scope` narrows retrieval; absent means the whole library the caller can read.
  Under Phase 7 that means the ACL filter applies to retrieval too — the same
  node-side filtering rule as semantic search, for the same reason.
- Retrieval only ever reaches content the caller could already open, so chat
  never becomes a way to read around a permission.

### Phase 9 — Scale & resilience

Mostly ops, so the API surface is small: it exists to let the admin console show
what the runbooks already describe.

**Storage health.** Admin only.

```
GET /admin/storage
```

```json
{
  "pool": { "name": "tank", "state": "ONLINE", "used_bytes": 0, "total_bytes": 0,
            "last_scrub_at": "...", "last_scrub_errors": 0 },
  "backup": { "last_success_at": "...", "last_failure_at": null, "age_seconds": 3600 },
  "tiers": [ { "name": "hot", "bytes": 0, "files": 0 },
             { "name": "cold", "bytes": 0, "files": 0 } ],
  "jobs": { "queued": 0, "running": 0, "failed": 0 }
}
```

Read from the same sources the alerts use — the zpool textfile collector, restic's
success timestamp, the jobs table — rather than a second, parallel notion of
health. Two systems disagreeing about whether the pool is fine is worse than one.

**Tiering.** Cold storage is a *location* change, never a visibility one: a
tiered file is still listed, still searchable, still has its metadata. Only its
bytes move.

| Route | Body / result |
|---|---|
| `GET /admin/tiering` | current policy |
| `PUT /admin/tiering` | `{min_age_days, min_size_bytes, exclude_tags: []}` |
| `POST /nodes/{id}/restore-tier` | pull a cold file back to hot; `202` |

- `GET /nodes/{id}/content` on a cold file either streams it transparently or,
  when the backend cannot do that within the request, returns
  `202 restore_in_progress` with `Retry-After`. Clients must handle 202 on the
  download path — a spinner, not an error.
- A node carries `"storage_tier": "hot"|"cold"|"restoring"` so a UI can warn
  before a click that will take minutes rather than after.

**Quota.** `GET /usage` already exists per user and is unchanged. Adding:

```
GET  /admin/quotas                 → {users: [{username, quota_bytes, used_bytes}]}
PUT  /admin/users/{id}/quota       {quota_bytes}   (0 = unlimited)
```

Enforcement stays where it is today — a `507 quota_exceeded` at write time, which
WebDAV clients already understand. No new failure mode for clients to learn.

---

## Contract test

The shared safety net named in the roadmap. It asserts the **real server** matches
this document:

- every route listed under "Shipped surface" exists and does not 405;
- unauthenticated routes are reachable without credentials, and every other route
  answers `401` without them — the check that a route was never accidentally left
  open;
- error responses parse as the nested `{error:{code,message}}` shape;
- documented response fields are present, and `sha256`/`blake3` are mutually
  exclusive on a node.

It lives behind the API (Guru's side) but is the one suite whose failure means
*the other track was misled*, so it is worth failing loudly and specifically.

---

*As each endpoint is designed, replace its `TBD` with the concrete request and
response shape. That edit is the handshake between the two tracks.*
