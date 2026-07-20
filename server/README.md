# private-cloud API

Go backend for the private cloud. **Phase 1, slice 1: skeleton.**

What exists: configuration, database pool, embedded migrations, health probes,
Prometheus metrics, structured logging, graceful shutdown. What doesn't: auth,
files, uploads, WebDAV, search — slices 2 through 7. See
[../docs/phase-1-design.md](../docs/phase-1-design.md).

## Layout

```
cmd/api/              entrypoint, graceful shutdown, healthcheck subcommand
internal/config/      env loading + eager validation
internal/db/          pgx pool, goose migrations (embedded)
internal/db/migrations/
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

## cloudctl

```bash
cloudctl user list                       # includes a (!) on users with 0 passkeys
cloudctl user create <name> [--admin]    # prints recovery codes
cloudctl user reset-auth <name>          # the lockout escape hatch
cloudctl user disable|enable <name>
cloudctl recovery regenerate <name>
cloudctl cleanup
```

`reset-auth` clears passkeys, revokes live sessions, and issues new recovery
codes together — clearing credentials while leaving sessions running would be a
half-measure.

## Development

```bash
make check    # fmt + vet + test — run before every commit
make test     # go test -race ./...
make run      # against a database on localhost:5432
make docker   # build the container image
```

No Go installed? Everything works in a container:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.25-alpine \
  sh -c "go vet ./... && go test ./..."
```

## Configuration

All via environment; validated at startup so a bad value fails immediately
rather than on the first request that touches it.

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

The database password is masked by `Config.Redacted()` before any logging, and
there's a test asserting it — so a future refactor can't quietly leak it into
Loki.

## Migrations

SQL under `internal/db/migrations/`, embedded into the binary with `go:embed`.
The container therefore can't drift from the schema its code expects, and
there's no "did you copy the migrations directory" failure mode.

Applied automatically at startup. Slice 1 ships only extensions; the domain
schema lands with auth in slice 2.

## Metrics

Exposed on `/metrics`, scraped directly by Prometheus (not through Caddy, so a
Caddy outage doesn't blind the monitoring).

- `privatecloud_http_requests_total{route,method,status}`
- `privatecloud_http_request_duration_seconds{route,method}`
- `privatecloud_http_requests_in_flight`
- `privatecloud_db_pool_acquired_connections`
- `privatecloud_schema_version`
- `privatecloud_build_info{version,commit}`
- Go runtime + process collectors

**The `route` label is the ServeMux pattern, never the raw path.** Labelling by
raw path would create a time series per filename and destroy Prometheus; there
are tests enforcing this, including one that unmatched paths collapse to a
single `unmatched` label so a port scanner can't inflate cardinality.

Alert rules for these live in
[../deploy/monitoring/alerts.yml](../deploy/monitoring/alerts.yml) under the
`api` group.
