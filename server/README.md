# private-cloud API

Go backend for the private cloud. **Phases 1–4 complete; Phase 5 in progress.**

Configuration, database pool, embedded migrations, health probes, Prometheus
metrics, structured logging, graceful shutdown, passkey auth, the file tree
(folders, downloads with Range support, trash, quotas, garbage collection and
fsck), resumable uploads over tus, a React web UI, WebDAV, and filename search
(Phase 1); a content-addressed storage engine with chunk-level dedup, real
version history and public share links (Phase 2); a change journal, a block-level
delta protocol and lineage-based conflict resolution for the sync client
(Phase 3); and a job queue with a separate `pcworker` process driving OCR,
semantic search, auto-tagging, plus OIDC single sign-on and a hardening pass
(Phase 4).

**Phase 5 (photos & media) is partially built and not yet reachable over HTTP.**
The schema, the pure `media` package and the metadata/variant store exist and are
tested; the job kind is not registered with the worker, nothing enqueues it, and
no route serves variants, the timeline or albums. See
[../docs/phase-5-design.md](../docs/phase-5-design.md) for what remains.

The endpoint table below lists what this server actually serves today;
[../docs/openapi.yaml](../docs/openapi.yaml) is the machine-readable form of the
same list, generated from the route table and enforced by a contract test.

The web UI lives in [../web/](../web/) and is served by Caddy from the same
origin as this API — not for convenience, but because WebAuthn binds passkeys
to an origin.

## Layout

```
cmd/api/              entrypoint, graceful shutdown, healthcheck subcommand
cmd/pcworker/         the job-queue worker: OCR, embeddings, tagging
cmd/cloudctl/         admin CLI: users, recovery, fsck, gc, jobs
internal/config/      env loading + eager validation
internal/db/          pgx pool, goose migrations (embedded)
internal/db/migrations/
internal/auth/        users, passkeys, recovery codes, sessions, OIDC
internal/blob/        opaque content storage on the filesystem
internal/cas/         content-defined chunking (FastCDC), BLAKE3, zstd
internal/files/       the tree: nodes, versions, trash, quota, GC, fsck, delta, journal
internal/shares/      public share links and their token plane
internal/jobs/        the queue: SKIP LOCKED claim, retry/backoff, reaper
internal/extract/     text decode, PDF text layer, tesseract OCR, auto-tagging
internal/embed/       vector math, chunking, inference-sidecar client
internal/media/       EXIF, thumbnail/preview rendering (Phase 5, not yet served)
internal/webdavfs/    webdav.FileSystem over the node store
internal/httpapi/     routing, middleware, handlers
internal/metrics/     Prometheus registry
```

Routing is the standard library's `ServeMux`. Since Go 1.22 it handles
method-and-wildcard patterns, so there is no third-party router — and nothing
between the network and the auth code in slice 2.

## Endpoints

| Route | Auth | Purpose |
|---|---|---|
| `GET /healthz` | — | Liveness. **Never touches the database.** |
| `GET /readyz` | — | Readiness. Pings the database. |
| `GET /metrics` | — | Prometheus exposition |
| `GET /api/v1/version` | — | Build metadata |
| `GET /api/v1/auth/status` | — | Is bootstrap needed? |
| `POST /api/v1/auth/register/{begin,finish}` | mixed | Enrol a passkey |
| `POST /api/v1/auth/login/{begin,finish}` | — | Sign in |
| `POST /api/v1/auth/recovery/redeem` | — | Redeem a recovery code |
| `POST /api/v1/auth/logout` | — | Revoke current session |
| `GET /api/v1/auth/me` | session | Current user |
| `GET|DELETE /api/v1/auth/credentials[/{id}]` | session | Manage passkeys |
| `GET|DELETE /api/v1/auth/sessions[/{id}]` | session | Manage devices |
| `POST /api/v1/auth/recovery/regenerate` | session | New recovery codes |
| `GET /api/v1/nodes/root` | session | The user's root folder |
| `GET /api/v1/nodes/resolve?path=/a/b` | session | Resolve an absolute path |
| `GET /api/v1/nodes/{id}` | session | Node metadata |
| `GET /api/v1/nodes/{id}/children` | session | List a folder |
| `PATCH /api/v1/nodes/{id}` | session | Rename and/or move |
| `DELETE /api/v1/nodes/{id}` | session | Move to trash |
| `POST /api/v1/folders` | session | Create a folder |
| `POST /api/v1/upload?parent_id=&name=` | session | Upload (raw body or multipart) |
| `GET|HEAD /api/v1/nodes/{id}/content` | session | Download; Range + ETag |
| `GET /api/v1/trash` | session | What's in the trash |
| `POST /api/v1/trash/{id}/restore` | session | Undelete |
| `DELETE /api/v1/trash/{id}` | session | Purge permanently |
| `DELETE /api/v1/trash` | session | Empty the trash |
| `GET /api/v1/usage` | session | Bytes used, quota, file count |
| `GET /api/v1/search?q=&semantic=` | session | Name, path and OCR'd content; `semantic=true` ranks by meaning |
| `POST /api/v1/admin/fsck[?repair=true]` | **admin** | Disk vs database audit |
| `OPTIONS /api/v1/uploads` | — | tus capabilities |
| `POST /api/v1/uploads` | session | Start a resumable upload |
| `HEAD /api/v1/uploads/{id}` | session | Where to resume from |
| `PATCH /api/v1/uploads/{id}` | session | Append a chunk |
| `DELETE /api/v1/uploads/{id}` | session | Abandon an upload |
| `GET`, `POST /api/v1/auth/app-passwords` | session | List / issue WebDAV credentials |
| `DELETE /api/v1/auth/app-passwords/{id}` | session | Revoke one |
| `POST /api/v1/auth/token` | **app password** | Exchange for a device bearer token |
| `GET /api/v1/auth/oidc/login`, `/callback` | — | Single sign-on, when configured |
| `GET /api/v1/nodes/{id}/versions` | session | Version history |
| `POST /api/v1/nodes/{id}/versions/{vid}/restore` | session | Restore a past version as the new head |
| `GET\|HEAD /api/v1/nodes/{id}/versions/{vid}/content` | session | Fetch one past version's bytes |
| `GET /api/v1/changes` | session | The change journal, by cursor |
| `GET /api/v1/nodes/{id}/manifest` | session | A file's chunk manifest |
| `POST /api/v1/chunks/have` | session | Negotiate which chunks are missing |
| `GET\|PUT /api/v1/chunks/{hash}` | session | Move one chunk; `PUT` re-verifies BLAKE3 |
| `POST /api/v1/manifests` | session | Commit a manifest into a file |
| `GET\|POST /api/v1/nodes/{id}/tags` | session | Per-node tags |
| `DELETE /api/v1/nodes/{id}/tags/{tag}` | session | Remove one |
| `GET /api/v1/tags`, `GET /api/v1/tags/{tag}` | session | Tag list with counts / filter by tag |
| `POST /api/v1/nodes/{id}/shares` | session | Create a public share link |
| `GET /api/v1/shares`, `DELETE /api/v1/shares/{id}` | session | List / revoke |
| `GET /api/v1/s/{token}` | — | Public share view (separate Caddy plane) |
| `POST /api/v1/s/{token}/unlock` | — | Password-protected share |
| `GET\|HEAD /api/v1/s/{token}/content` | — | Public share download |
| `/dav/*` | **app password** | WebDAV — mount as a network drive |

`{id}` accepts the literal `root`, so a client can start browsing without a
prior lookup.

`/healthz` and `/readyz` are split on purpose. Docker restarts a container whose
healthcheck fails; if liveness depended on Postgres, a brief database blip would
restart the API, turning a recoverable hiccup into a crash loop.

## Auth model

Passkeys (WebAuthn) only — there is no password anywhere in the system.

**Because there is no password, lockout is the real risk.** Lose the
authenticator and you lose your own file server. Three independent escapes:

1. **Register several passkeys** (laptop, phone, hardware key). The API refuses
   to delete your last one.
2. **Recovery codes** — 10 per user, 100 bits each, argon2id-hashed, shown
   exactly once. Redeeming one yields a session that can do *nothing* except
   enrol a new passkey and expires in 15 minutes.
3. **`cloudctl user reset-auth`** on the server itself. Requires shell access,
   which already implies database and file access, so it weakens nothing.

Sessions are server-side rows, not JWTs — revocation has to be immediate, and a
JWT stays valid until it expires no matter how urgently you want it dead.

Account creation is deliberately **not** a public endpoint. The first passkey to
arrive when the users table is empty becomes the admin; everyone after that is
created with `cloudctl user create`, which prints recovery codes they use to
sign in once and then enrol a passkey.

### The one setting that will bite you

`PC_WEBAUTHN_RPID` is the bare domain passkeys bind to — **no scheme, no port,
no path**. Config validation rejects those, because a wrong RPID fails in the
browser with an error that tells you nothing.

**Changing RPID invalidates every enrolled passkey.** Settle on the final
hostname (the MagicDNS name) before enrolling keys you care about.

## Storage model

```
node ──► file_version ──► blob ──► bytes on disk
```

That indirection was the **Phase 2 shape adopted early**, and it paid: Phase 1
wrote exactly one version per file and stored whole files, so adding history and
content-defined chunking in Phase 2 was a data migration rather than a schema
rewrite over live data.

Since Phase 2 a file is normally a **manifest** of content-defined chunks
(FastCDC, BLAKE3-addressed, zstd-compressed), deduplicated across users. Whole
blobs still exist — they are what Phase 1 wrote, and the background migration
converts them — which is why a `node` carries `sha256` **xor** `blake3` and never
both. Whole blobs keep a random storage key rather than a content hash: the hash
isn't known until the stream has been read, and buffering an arbitrary upload to
learn it first isn't acceptable. Content addressing lives at the chunk level,
where it belongs.

**Bytes are written before the database row.** A crash between the two leaves an
unreferenced blob, which GC reclaims and `fsck` reports. The opposite ordering
would leave a file the UI lists, the API describes and nobody can download — a
failure no background job can repair, because the bytes were never written.

Paths are materialised on every node (`/photos/2026/img.jpg`). Denormalised on
purpose: subtree reads become a prefix scan, which is what search in slice 7 and
inherited ACLs in Phase 2 both want. Rename pays for it by rewriting
descendants, in the same transaction.

Sibling uniqueness is enforced on a **folded** (lowercased) name. Linux is
case-sensitive, macOS and Windows are not, and a WebDAV client from either will
try to create `Photos` beside `photos`. Allowing both produces a pair of entries
only some clients can see.

Deleting is a soft delete that stamps the whole subtree, plus a `trashed_root_id`
recording which node the user actually deleted. Without that column, a folder
deleted as one unit is indistinguishable from its contents having been deleted
individually beforehand — and restoring it would resurrect files the user had
already thrown away.

Trashed files keep consuming quota until purged. That's honest: the bytes really
are still on the disk.

## Resumable uploads

`POST /api/v1/upload` is fine for small files. A 4 GB video over a phone
connection will fail eventually, and starting over from zero is the difference
between a usable file server and a toy — so large uploads go through
**tus 1.0.0** (core + creation + termination + expiration).

tus rather than a bespoke chunk protocol because the clients already exist: uppy
and tus-js-client in the browser, tus-py-client elsewhere. Inventing our own
framing would mean writing every one of those clients too, and getting
resumption subtly wrong in each.

```
POST   /api/v1/uploads      Upload-Length: 4294967296
                            Upload-Metadata: filename <b64>,parent_id <b64>
  -> 201, Location: /api/v1/uploads/<id>

PATCH  /api/v1/uploads/<id> Upload-Offset: 0
                            Content-Type: application/offset+octet-stream
  -> 204, Upload-Offset: 1048576

HEAD   /api/v1/uploads/<id>  -> 200, Upload-Offset: <resume here>
```

Three things make this survive a crash rather than merely a disconnect:

- **The declared length is required.** `Upload-Defer-Length` is unsupported,
  because without a size there is no way to check quota before accepting bytes,
  and discovering at 99% that a file will not fit is the worst possible moment.
- **The content hash is computed incrementally.** `crypto/sha256` implements
  `BinaryMarshaler`, so the digest state is suspended between requests instead
  of re-reading a finished multi-gigabyte file just to learn its hash. The state
  and the offset are written in **one statement** — split across two, a crash
  between them would leave a hash that does not describe the bytes the offset
  claims.
- **The staging file is truncated to the committed offset before every append.**
  A crash after writing bytes but before committing progress leaves the file
  longer than the record. Truncating makes the recorded offset the single
  authority, so a resumed upload cannot splice a duplicated chunk into the
  middle of a file.

Concurrent `PATCH`es are rejected with `423 Locked`. The lock is a
`locked_until` timestamp refreshed while a chunk is in flight, not
`SELECT ... FOR UPDATE`: a row lock would have to be held inside a transaction
for the whole transfer, tying up a pool connection for as long as the client's
network takes.

Finishing is automatic on the last byte. tus has no commit step, and adding one
would strand completed uploads whenever a client disconnected right after its
final chunk. The created node is reported in `X-Node-Id` / `X-Node-Path`,
because tus reserves the PATCH response body.

## WebDAV

Mount the tree as a network drive at `https://<host>/dav/`. No client to
install, no sync daemon, and every application on the machine can open files
directly — the cheapest possible "works with everything" surface, and the
reason the node store grew a path-addressed API in slice 3.

```bash
# macOS:   Finder -> Go -> Connect to Server -> https://<host>/dav/
# Windows: Explorer -> Map network drive -> https://<host>/dav/
# Linux:   gio mount dav://<host>/dav/
rclone config   # type: webdav, vendor: other, url: https://<host>/dav/
```

Authentication is **HTTP Basic with an app password**, because a filesystem
driver cannot run a WebAuthn ceremony. Create one in Settings, then sign in with
your username and that password. It is shown once and stored as argon2id, like
a recovery code.

This is a real weakening of the auth model, and it is contained deliberately:

- One password per client, named and individually revocable. Losing a laptop
  revokes one credential rather than forcing a global reset.
- **Scoped to `/dav` only.** An app password cannot call the JSON API, so a
  leaked one cannot enrol a passkey, read recovery codes, or change how you
  sign in. It reaches files, which is bad enough; it cannot take the account.
- Rate limited like the auth endpoints — Basic auth over WebDAV is the one
  place in this system where online guessing against a secret is possible.
- `cloudctl user reset-auth` revokes them alongside passkeys and sessions.

The credential is `pcap_<lookup>_<secret>`. The `pcap_` prefix makes a leaked
one greppable by secret scanners and by a human reading a config file. The
lookup half is stored in clear and indexed so verification hashes exactly one
candidate — a WebDAV client issues a great many requests, and argon2id at
64 MiB per candidate is not something to do in a loop.

`DELETE` over WebDAV moves to the trash rather than purging. A client deleting
the wrong folder should be a recoverable accident.

Locks are held in memory. They exist to stop two clients writing one file at
once; losing them on restart is the same thing that happens when a client's
connection drops. A database-backed lock table stays deferred until there is
more than one replica — which the single-server design does not plan for.

## Search

`GET /api/v1/search?q=budg` with optional `kind`, `under`, `limit`, `offset`,
`include_trashed` and `semantic`.

Three layers, added in that order: **names and paths** (Phase 1, trigram),
**extracted content** (Phase 4 — a scanned receipt is findable by a word printed
on it, flagged as `matched_content`), and **meaning** (Phase 4 —
`semantic=true` embeds the query and ranks by cosine, answering `503
semantic_unavailable` when no inference sidecar is wired, which clients treat as
"retry lexically *and say so*").

**Trigram (`pg_trgm`) for names, deliberately not full-text.** `to_tsvector` stems
words and matches on token boundaries — it is the right tool for document
*content*, and that is exactly what it is used for. But filenames are not prose.
People search them by fragment: `budg` should find `budget-2026-final.xlsx`,
and so should `2026`. Full-text search finds neither, because neither is a
token in that filename.

That requires an unanchored `LIKE '%frag%'`, which no btree index can help
with — without the GIN trigram indexes in migration `00006`, every search is a
sequential scan of the entire tree.

Ranking is exact match, then prefix, then trigram similarity, then most
recently updated. Someone typing a full filename wants *that* file, not the
forty others containing it as a substring; and `budg` ranking `budget.xlsx`
above `old-budget.xlsx` matches how people think about names.

Paths are matched as well as names, so `photos` finds everything under
`/photos` without the caller having to know whether the fragment spans a
directory boundary. Results carry `matched_path` so a filename with no visible
relationship to the query does not look like a bug.

Queries shorter than two characters are refused rather than served: pg_trgm
indexes trigrams, so a single character cannot use the index at all.

The trash is excluded by default. Finding a file you deleted last month and
being unable to tell that it is deleted is worse than not finding it.

## cloudctl

```bash
cloudctl user list                       # includes a (!) on users with 0 passkeys
cloudctl user create <name> [--admin]    # prints recovery codes
cloudctl user reset-auth <name>          # the lockout escape hatch
cloudctl user disable|enable <name>
cloudctl recovery regenerate <name>
cloudctl cleanup                         # expired sessions and ceremonies
cloudctl fsck [--repair]                 # compare the blob store to the database
cloudctl gc                              # purge expired trash, free unreferenced blobs
cloudctl jobs reindex [--kind=]          # re-enqueue extraction/embedding over the tree
```

`reset-auth` clears passkeys, revokes live sessions, and issues new recovery
codes together — clearing credentials while leaving sessions running would be a
half-measure.

`fsck` exits non-zero when it finds missing or mismatched content, so a cron
wrapper notices without parsing text. It never deletes a database row: a report
saying "these bytes are gone" is far more useful than a tool that quietly erases
the record of a file you still expect to exist. `--repair` only removes things
that are provably unreferenced.

## Development

```bash
make check    # fmt + vet + test — run before every commit
make test     # go test -race ./...
make run      # against a database on localhost:5432
make docker   # build the container image
```

No Go installed? Everything works in a container. The named volumes matter —
without them every run re-downloads the module graph:

```bash
docker run --rm -v "$PWD:/src" -w /src \
  -v pc-gomod25:/go/pkg/mod -v pc-gobuild25:/root/.cache/go-build \
  golang:1.25-alpine sh -c "go vet ./... && go test ./..."
```

### Integration tests

The tree logic lives almost entirely in SQL — partial unique indexes, cascades,
the refcount trigger, prefix rewrites on rename. None of that is exercised by a
mock: a fake store would pass while the real schema silently allowed duplicate
siblings. So those tests run against a real Postgres and skip without one.

```bash
docker network create pc-test
docker run -d --name pc-test-pg --network pc-test \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=privatecloud postgres:17.5-alpine

docker run --rm --network pc-test -v "$PWD:/src" -w /src \
  -v pc-gomod25:/go/pkg/mod -v pc-gobuild25:/root/.cache/go-build \
  -e PC_TEST_DATABASE_URL="postgres://postgres:test@pc-test-pg:5432/privatecloud?sslmode=disable" \
  golang:1.25-alpine go test ./... -count=1

docker rm -f pc-test-pg && docker network rm pc-test
```

## Configuration

All via environment; validated at startup so a bad value fails immediately
rather than on the first request that touches it. The table below is the core
set from Phase 1; the CAS, worker, embedding-sidecar and OIDC settings added
since are declared alongside it in
[internal/config/config.go](internal/config/config.go), which is the complete
list by construction.

| Variable | Default | Notes |
|---|---|---|
| `PC_DATABASE_URL` | — | **Required.** Must be a `postgres://` URL |
| `PC_HTTP_ADDR` | `:8080` | |
| `PC_ENV` | `dev` | `dev` or `prod` |
| `PC_LOG_LEVEL` | `info` | debug/info/warn/error |
| `PC_LOG_FORMAT` | `json` | `text` is easier to read while developing |
| `PC_MIGRATE_ON_START` | `true` | Correct for one node; wrong with replicas racing |
| `PC_DB_MAX_CONNS` | `10` | |
| `PC_SHUTDOWN_TIMEOUT` | `20s` | Drain window for in-flight requests |
| `PC_BLOB_PATH` | `/data/blobs` | **Must be absolute**, and inside the ZFS dataset that sanoid and restic cover |
| `PC_TRASH_RETENTION` | `720h` | How long deleted files survive |
| `PC_BLOB_GC_INTERVAL` | `6h` | How often expired trash and orphan content are swept |
| `PC_UPLOAD_TTL` | `48h` | How long an abandoned resumable upload occupies disk |

The database password is masked by `Config.Redacted()` before any logging, and
there's a test asserting it — so a future refactor can't quietly leak it into
Loki.

## Migrations

SQL under `internal/db/migrations/`, embedded into the binary with `go:embed`.
The container therefore can't drift from the schema its code expects, and
there's no "did you copy the migrations directory" failure mode.

Applied automatically at startup.

| Version | Contents |
|---|---|
| `00001` | extensions (`pg_trgm`) |
| `00002` | users, passkeys, recovery codes, sessions, ceremonies |
| `00003` | blobs, nodes, file versions, refcount trigger |
| `00004` | resumable upload sessions |
| `00005` | app passwords |
| `00006` | trigram indexes for search |
| `00007` | CAS: chunks, manifests, manifest entries |
| `00008` | blob refcount trigger covers updates |
| `00009` | share links |
| `00010` | the change journal |
| `00011` | the job queue |
| `00012` | `doc_text` (content-addressed extracted text) |
| `00013` | `doc_embedding` (content-addressed packed float32 vectors) |
| `00014` | `node_tags` (auto + user, `source`-tracked) |
| `00015` | OIDC identities |
| `00016` | case-folded path index for search |
| `00017` | job claim by kind |
| `00018` | job state and dead-lettering |
| `00019` | media metadata and variants (Phase 5) |
| `00020` | albums and album items (Phase 5) |

Blob refcounts are maintained by a **trigger**, not by application code.
`file_versions` rows disappear through `ON DELETE CASCADE` — purging a folder
destroys versions several levels down that no Go code ever names — so anything
keeping the count in the service layer would have to reimplement the cascade to
know what it just deleted, and would drift the first time a new delete path
appeared. An undercount is the dangerous direction: it lets GC delete bytes a
live file still points at.

## Metrics

Exposed on `/metrics`, scraped directly by Prometheus (not through Caddy, so a
Caddy outage doesn't blind the monitoring).

- `privatecloud_http_requests_total{route,method,status}`
- `privatecloud_http_request_duration_seconds{route,method}`
- `privatecloud_http_requests_in_flight`
- `privatecloud_db_pool_acquired_connections`
- `privatecloud_schema_version`
- `privatecloud_build_info{version,commit}`
- `privatecloud_upload_bytes_total` / `privatecloud_download_bytes_total`
- `privatecloud_gc_blobs_freed_total` / `privatecloud_gc_bytes_freed_total`
- Go runtime + process collectors

Transfer counters are separate from the duration histogram deliberately: bytes
moved and time taken answer different questions, and a 60-second upload sharing
a histogram with a 5ms metadata lookup makes both unreadable.

**The `route` label is the ServeMux pattern, never the raw path.** Labelling by
raw path would create a time series per filename and destroy Prometheus; there
are tests enforcing this, including one that unmatched paths collapse to a
single `unmatched` label so a port scanner can't inflate cardinality.

Alert rules for these live in
[../deploy/monitoring/alerts.yml](../deploy/monitoring/alerts.yml) under the
`api` group.
