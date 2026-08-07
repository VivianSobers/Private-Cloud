# Phase 4 — Hardening Pass

**Status: done (slice 5).** Not a feature — a deliberate second walk of a surface
that has grown a great deal since Phase 1, done once, on purpose, while the whole
system is fresh in mind. This document is the record of that walk: what was
reviewed, what was fixed, and what was consciously left as acceptable.

---

## 1. Dependency and secret audit

- **`govulncheck` is clean of called vulnerabilities.** The pass found one that
  reached our code — **GO-2026-5004**, a SQL-injection via placeholder confusion
  in `github.com/jackc/pgx/v5` (our whole data layer) — and it was fixed by
  upgrading pgx `v5.7.2 → v5.9.2`. The full test suite is green on the new
  version. One advisory remains in a required-but-**uncalled** module; govulncheck
  confirms no code path reaches it, so it is tracked, not urgent, and will clear
  on the next routine `go get -u`.
- **Secrets load from env/secret material, never defaults, never logs.** The OIDC
  client secret and the database URL are read from the environment; the config's
  `Redacted()` view redacts the DB URL, and startup logs the OIDC *issuer* only,
  never the secret. The embedding sidecar URL carries no credential.
- **New ML dependencies are justified and pinned.** `ledongthuc/pdf` (pure-Go PDF
  text, no cgo), `coreos/go-oidc` + `golang.org/x/oauth2` (vetted auth libraries,
  chosen precisely so JWT/JWKS verification is not hand-rolled). OCR shells out to
  the `tesseract` binary as a subprocess — no cgo, and a crash there takes the
  subprocess, not the worker.

## 2. Endpoint abuse review (everything added since Phase 1)

| Surface | Reviewed for | Verdict |
|---|---|---|
| Share plane (`/s/*`) | Revocation immediacy, password guessing, owner-identity leak | Row-based (instant revoke), argon2id password, rate limited, leak-free — as designed in Phase 2 |
| Delta chunks (`PUT/GET /chunks`) | Address forgery, cross-user read | `PUT` recomputes BLAKE3 and rejects mismatch; `GET` scoped to a referencing user (404 otherwise) — as designed in Phase 3 |
| Device-token exchange (`/auth/token`) | Privilege escalation from an app password | Rate limited; the minted device session is confined away from credential management |
| Job queue (enqueue on upload) | One user flooding the single worker | `OwnerQueueCap` bounds queued jobs per owner; the unique-pending index dedups per (kind, node) |
| OCR / extraction | A crafted file pinning the worker | 64 MiB input cap, per-job OCR timeout, PDF read bounded and panic-guarded; a bad file is a skip, never a crash |
| Extracted text | Stored-XSS / log-injection back-channel | `doc_text` is used only for matching — it is never returned by the API and never logged |
| Semantic search | Cross-space comparison, corpus blow-up | Vectors filtered by model **and** dimension; the exact-scan is bounded by `maxSemanticScan` |
| Tags | Injection on display | User tags reject control characters and are length-bounded; auto tags come from a controlled vocabulary + MIME |
| OIDC login | Token forgery, CSRF, replay, account takeover | Verification via go-oidc; state (CSRF), nonce (replay) and PKCE bound through a single-use flow cookie; provisions its own users, never auto-links by email |

## 3. Fixes landed in this slice

- **pgx upgraded** to clear GO-2026-5004 (see §1).
- **Request-body cap** (`withBodyLimit`, 2 MiB) on every endpoint except the ones
  that legitimately stream large content (upload, chunk `PUT`, resumable `PATCH`,
  WebDAV). Previously a metadata endpoint would buffer and decode an
  unbounded JSON body — an OOM lever on a 7 GiB box.
- **Baseline security headers** (`withSecurityHeaders`) on every response:
  `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
  `Referrer-Policy: no-referrer`. The file-download path keeps its own stricter
  `Content-Security-Policy: sandbox`.
- **Tag input validation**: user tags with control characters are refused at the
  store, closing a display/log-injection vector.

## 4. Resource-exhaustion review, weighted for this box

The failure modes a 7 GiB machine hits first, and where each is bounded:

- **Worker memory** — one job at a time by default; the heavy model runs in the
  separate sidecar (on a GPU box), never in the API or, resident, in the worker.
  Stopping the worker is the operator's ceiling lever.
- **OCR/PDF** — input size cap, page/read bounds, and a per-job timeout, so one
  file cannot occupy the single worker slot indefinitely.
- **Vector scan** — bounded by `maxSemanticScan`; beyond personal scale the design
  calls for pgvector, and the query layer is written to adopt it without a schema
  change.
- **Upload path** — `MaxBytesReader`, not a trusted `Content-Length`, so a client
  cannot claim one byte and send a terabyte.

## 5. Consciously accepted / deferred

- **Per-user API rate limiting** (beyond the per-IP auth limiter and the enqueue
  cap) is not yet in place. An authenticated user could still issue many semantic
  queries, each an RPC to the sidecar. On a single-user tailnet this is low risk;
  a per-user token bucket on the expensive endpoints is the right next step if the
  deployment grows past one trusted user.
- **The uncalled dependency advisory** in §1 is tracked, not patched, because no
  code path reaches it.
- **pgvector** is deferred until a corpus outgrows the exact scan, by design.
