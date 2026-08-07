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

> Fill in from the current handlers as the contract test is written. Groups that
> exist today:

- **Auth** — `POST /auth/token`, `GET /auth/status`, `GET /auth/me`,
  `POST /auth/logout`, passkey + OIDC login/callback.
- **Files/nodes** — list, get, upload, download, move, trash/restore, versions.
- **Sync** — `GET /changes`, manifest fetch/commit, chunk have/get/put.
- **Search** — lexical + optional `semantic` mode; results carry
  `matched_content` / `semantic` / `score`.
- **Tags** — list, tag a node, node tags, add/remove.
- **Shares** — public share links (file & folder).

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
