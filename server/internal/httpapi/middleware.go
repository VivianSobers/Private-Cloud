package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the correlation ID for a request, or "" outside one.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusRecorder captures the status code, which net/http otherwise gives no
// way to read back after the handler has written it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wrote {
		return // a double WriteHeader is a handler bug; don't compound it
	}
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		// Mirror net/http: an implicit 200 on first write.
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// streaming endpoints in slice 3 will need for flushing and deadline control.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withRequestID assigns each request a correlation ID, preferring an inbound
// X-Request-Id so a trace survives the hop through Caddy.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failing CSPRNG is fatal for auth later, but for a log correlation
		// ID a timestamp is an acceptable degradation.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// withRecovery turns a handler panic into a 500 instead of killing the process.
// Without this, one nil dereference in any handler takes the whole server down.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the documented way for a handler to
				// abort a response; it is not a bug and must not be logged as
				// one or it will drown real panics in noise.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic in handler",
					"error", rec,
					"request_id", RequestID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withObservability records metrics and one structured log line per request.
//
// Metrics are labelled with r.Pattern — the ServeMux pattern that matched, e.g.
// "GET /api/v1/nodes/{id}" — not the concrete path. See metrics.go.
func (s *Server) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		s.metrics.HTTPInFlight.Inc()
		defer s.metrics.HTTPInFlight.Dec()

		next.ServeHTTP(rec, r)

		elapsed := time.Since(start)
		route := r.Pattern
		if route == "" {
			// Unmatched requests (404s) share one label rather than one per
			// probed URL — otherwise a scanner hitting random paths becomes a
			// cardinality attack on your monitoring.
			route = "unmatched"
		}

		s.metrics.HTTPRequests.WithLabelValues(route, r.Method, statusClass(rec.status)).Inc()
		s.metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())

		// Health checks fire every few seconds; logging them at info would bury
		// everything else.
		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case isProbe(r.URL.Path):
			level = slog.LevelDebug
		}

		s.log.Log(r.Context(), level, "http request",
			"request_id", RequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"route", route,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", elapsed.Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/metrics"
}

// maxJSONBody caps a request body on the metadata endpoints. Comfortably fits the
// largest legitimate JSON — a manifest commit or a chunk-existence query with the
// endpoint's own 10k-hash limit is well under this — while stopping an
// unbounded-body decode from exhausting memory on a 7 GiB box.
const maxJSONBody = 2 << 20 // 2 MiB

// withBodyLimit caps request bodies everywhere except the routes that legitimately
// stream large content, which enforce their own, larger limits (uploads, chunk
// puts, resumable PATCHes, WebDAV). Without this, a POST of a gigabyte of JSON to
// any metadata endpoint would be buffered and decoded in full.
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !largeBodyRoute(r) {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		}
		next.ServeHTTP(w, r)
	})
}

// largeBodyRoute reports whether a request legitimately carries a large body and
// so must be exempt from the metadata-body cap.
func largeBodyRoute(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/api/v1/upload":
		return true
	case r.Method == http.MethodPut && strings.HasPrefix(p, "/api/v1/chunks/"):
		return true
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/api/v1/uploads/"):
		return true
	case p == davPrefix || strings.HasPrefix(p, davPrefix+"/"):
		return true
	}
	return false
}

// withSecurityHeaders adds defense-in-depth headers to every response. The file
// download path sets its own stricter CSP/sandbox; these are the baseline for
// everything else, and never override what a handler set deliberately.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Never let a browser second-guess a declared content type — the defense
		// against a JSON or text response being sniffed as executable HTML.
		h.Set("X-Content-Type-Options", "nosniff")
		// The API is not meant to be framed; deny clickjacking outright.
		h.Set("X-Frame-Options", "DENY")
		// Do not leak API URLs (which can carry ids) in a Referer to any origin.
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// statusClass buckets statuses as 2xx/4xx/5xx. The exact code is in the logs;
// the metric only needs enough resolution to alert on, and full codes would
// multiply series count for no operational gain.
func statusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
