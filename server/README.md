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

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness. **Never touches the database.** |
| `GET /readyz` | Readiness. Pings the database. |
| `GET /metrics` | Prometheus exposition |
| `GET /api/v1/version` | Build metadata |

`/healthz` and `/readyz` are split on purpose. Docker restarts a container whose
healthcheck fails; if liveness depended on Postgres, a brief database blip would
restart the API, turning a recoverable hiccup into a crash loop.

## Development

```bash
make check    # fmt + vet + test — run before every commit
make test     # go test -race ./...
make run      # against a database on localhost:5432
make docker   # build the container image
```

No Go installed? Everything works in a container:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.23-alpine \
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
