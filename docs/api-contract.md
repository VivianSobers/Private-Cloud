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
- `GET /albums`, `POST /albums`, `PATCH /albums/{id}`, `GET /albums/{id}` — TBD shapes.
- Media metadata on nodes (EXIF: taken-at, gps, dimensions) — TBD.
- Thumbnail / variant retrieval — TBD (likely a param on the existing blob path).

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
