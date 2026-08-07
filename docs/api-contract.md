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
- `GET /devices`, `DELETE /devices/{id}` — list/revoke device tokens — TBD.

### Phase 7 — Multi-user & sharing
- User-to-user share + shared-folder endpoints — TBD.
- ACL-scoped variants of search/tags — TBD.
- Admin: users, quotas, sessions, audit — TBD.

### Phase 8 — Advanced intelligence
- `GET /people`, faces endpoints — TBD.
- `POST /chat` (RAG over the user's library) — TBD.
- Similar-files endpoint — TBD.

### Phase 9 — Scale & resilience
- Quota / usage endpoints; storage-health surfacing — TBD.

---

*As each endpoint is designed, replace its `TBD` with the concrete request and
response shape. That edit is the handshake between the two tracks.*
