package httpapi

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/metrics"
)

// The contract test.
//
// docs/roadmap-split.md calls contract tests "the shared safety net … the only
// place both tracks' assumptions meet, and it fails loudly when they drift."
// There was no such test, and the drift it was supposed to catch happened: the
// web client shipped whole views against Phase 5, 7 and 8 endpoints that do not
// exist, and nothing failed.
//
// The rule this file enforces is narrow on purpose. docs/api-contract.md is
// prose and deliberately describes *proposed* endpoints alongside shipped ones,
// so it cannot be the machine-checkable artifact. docs/openapi.yaml is: it is
// generated from the server's own route table and describes only what actually
// responds. This test regenerates it and fails if the committed file differs, so
// the spec can never claim an endpoint the mux does not serve, or miss one it
// does.
//
// What is checked here is existence, method and auth posture — the questions a
// client blocks on. Meaning stays in api-contract.md, because it is not
// derivable from a route table and a spec that restated it by hand would be one
// more thing to drift.

var updateOpenAPI = flag.Bool("update-openapi", false,
	"rewrite docs/openapi.yaml from the server's registered routes")

const openAPIPath = "../../../docs/openapi.yaml"

func newContractServer(t *testing.T) *Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "abc123", func() float64 { return 0 })
	return NewServer(log, nil, m, nil, nil, nil, Options{Version: "test", Commit: "abc123"})
}

func TestOpenAPISpecMatchesRegisteredRoutes(t *testing.T) {
	srv := newContractServer(t)
	generated := generateOpenAPI(srv)

	if *updateOpenAPI {
		if err := os.WriteFile(filepath.FromSlash(openAPIPath), []byte(generated), 0o644); err != nil {
			t.Fatalf("writing spec: %v", err)
		}
		t.Log("docs/openapi.yaml regenerated")
		return
	}

	committed, err := os.ReadFile(filepath.FromSlash(openAPIPath))
	if err != nil {
		t.Fatalf("reading docs/openapi.yaml: %v", err)
	}

	if string(committed) != generated {
		t.Errorf("docs/openapi.yaml is out of date with the registered routes.\n"+
			"Regenerate it with:\n\n"+
			"    go test ./internal/httpapi -run TestOpenAPISpecMatchesRegisteredRoutes -update-openapi\n\n"+
			"and commit the result alongside the route change.\n\n%s",
			firstDifference(string(committed), generated))
	}
}

// TestProposedSurfaceIsNotServedYet pins the gap between the two tracks.
//
// Every path below is specified in api-contract.md and called by code in web/,
// and none of them is implemented. That is a legitimate state — the split exists
// precisely so the front track can build ahead of the server — but it must be a
// *visible* one, and it must degrade the way the clients assume: a parseable
// JSON 404, never HTML, never a 500, never a hang.
//
// When one of these lands, this test fails. That is the intent: implementing an
// endpoint should force the same commit to move it into the generated spec and
// to update the roadmap in README.md, rather than leaving three documents
// disagreeing about whether the feature exists.
func TestProposedSurfaceIsNotServedYet(t *testing.T) {
	// Phase 5 is deliberately absent: its endpoints are implemented and now live
	// in the generated spec instead. Removing them from this list was part of the
	// commit that landed them, which is the workflow this test exists to force.
	pending := []struct {
		phase  string
		method string
		path   string
	}{
		{"6 — native clients", "GET", "/api/v1/devices"},
		{"7 — multi-user", "GET", "/api/v1/grants"},
		{"7 — multi-user", "GET", "/api/v1/shared"},
		{"7 — multi-user", "GET", "/api/v1/admin/users"},
		{"7 — multi-user", "GET", "/api/v1/admin/audit"},
		{"8 — intelligence", "GET", "/api/v1/people"},
		{"8 — intelligence", "POST", "/api/v1/chat"},
		{"9 — scale", "GET", "/api/v1/admin/storage"},
	}

	h := newContractServer(t).Handler()

	for _, p := range pending {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(p.method, p.path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 — %s appears to be implemented now.\n"+
					"Regenerate docs/openapi.yaml, drop this entry, and update the Phase %s "+
					"row in README.md in the same commit.",
					rec.Code, p.path, p.phase)
			}

			// The clients render "not available on this server yet" off this
			// body. If it stopped being the standard envelope they would show a
			// parse error instead of an explanation.
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("404 body is not the standard error envelope: %v", err)
			}
			if body.Error.Code != "not_found" {
				t.Errorf("error code = %q, want not_found", body.Error.Code)
			}
		})
	}
}

// TestDeclaredRouteFactsNameRealRoutes keeps the two hand-declared maps honest.
//
// Probing derives "does this need credentials", which is most of what a client
// needs, but it cannot see *which* credentials: an admin route and a Basic-auth
// route both answer 401 to an anonymous request. Those two facts are therefore
// declared, and this test asserts the declarations still name patterns that
// exist, so a rename cannot leave the spec quietly describing the wrong thing.
func TestDeclaredRouteFactsNameRealRoutes(t *testing.T) {
	registered := make(map[string]bool)
	for _, p := range newContractServer(t).Routes() {
		registered[p] = true
	}
	for _, decl := range []struct {
		name   string
		routes map[string]bool
	}{
		{"adminRoutes", adminRoutes},
		{"basicAuthRoutes", basicAuthRoutes},
	} {
		for pattern := range decl.routes {
			if !registered[pattern] {
				t.Errorf("%s names %q, which is not registered — rename or drop it", decl.name, pattern)
			}
		}
	}
}

// --- the generator -----------------------------------------------------------

// adminRoutes are the patterns that require an admin, not merely a session.
// Declared rather than probed; see TestDeclaredRouteFactsNameRealRoutes.
var adminRoutes = map[string]bool{
	"POST /api/v1/admin/fsck": true,
}

// basicAuthRoutes take an app password over HTTP Basic rather than a session.
//
// Only one JSON route does: the device-token exchange, which is how a headless
// client turns an app password into a bearer token. It probes as "needs
// credentials" like any authenticated route, so without this it would be
// documented as accepting a session cookie — and a sync client following that
// would never get past its first call. (WebDAV is the other Basic surface, but
// it is a prefix route and not an operation here.)
var basicAuthRoutes = map[string]bool{
	"POST /api/v1/auth/token": true,
}

// methodOrder fixes the order operations appear under a path, so the generated
// file is stable across runs regardless of registration order.
var methodOrder = []string{"get", "head", "post", "put", "patch", "delete", "options"}

type operation struct {
	method  string // lowercase
	pattern string // the original ServeMux pattern
	public  bool
}

var wildcardRe = regexp.MustCompile(`\{([^}]+)\}`)

func generateOpenAPI(srv *Server) string {
	h := srv.Handler()

	paths := map[string][]operation{}
	var prefixRoutes []string

	for _, pattern := range srv.Routes() {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			// A prefix route: WebDAV and the JSON 404 catch-all. Neither is an
			// OpenAPI operation — WebDAV is a different protocol with methods
			// OpenAPI has no vocabulary for, and the catch-all is the absence of
			// a route. Recorded rather than dropped so nothing leaves the route
			// table without appearing somewhere in the spec.
			prefixRoutes = append(prefixRoutes, pattern)
			continue
		}
		paths[path] = append(paths[path], operation{
			method:  strings.ToLower(method),
			pattern: pattern,
			public:  !requiresAuth(h, method, path),
		})
	}

	var b strings.Builder
	b.WriteString(specPreamble)

	sortedPaths := make([]string, 0, len(paths))
	for p := range paths {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	b.WriteString("paths:\n")
	for _, path := range sortedPaths {
		b.WriteString("  " + yamlKey(path) + ":\n")
		writePathParameters(&b, path)

		ops := paths[path]
		sort.Slice(ops, func(i, j int) bool {
			return methodRank(ops[i].method) < methodRank(ops[j].method)
		})
		for _, op := range ops {
			writeOperation(&b, path, op)
		}
	}

	b.WriteString("\n# Routes registered by prefix rather than by method pattern. They are served\n")
	b.WriteString("# but are not OpenAPI operations; see the description above.\n")
	b.WriteString("x-prefix-routes:\n")
	sort.Strings(prefixRoutes)
	for _, p := range prefixRoutes {
		b.WriteString("  - " + yamlKey(p) + "\n")
	}

	b.WriteString(specComponents)
	return b.String()
}

// requiresAuth probes the real handler rather than trusting a list.
//
// An unauthenticated request carries no session cookie, and Authenticate returns
// early on an empty token, so a requireAuth-wrapped route answers 401 without
// reaching a service or a database. Anything else — including the 429 a rate
// limiter returns and the 500 a nil service produces in this test build — means
// the request was not gated on a session, which is exactly the question being
// asked. Deriving this rather than declaring it is what stops the spec claiming
// a route is protected after somebody drops the wrapper.
func requiresAuth(h http.Handler, method, path string) bool {
	concrete := wildcardRe.ReplaceAllString(path, "00000000-0000-0000-0000-000000000000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, concrete, nil))
	return rec.Code == http.StatusUnauthorized
}

func writePathParameters(b *strings.Builder, path string) {
	names := wildcardRe.FindAllStringSubmatch(path, -1)
	if len(names) == 0 {
		return
	}
	b.WriteString("    parameters:\n")
	for _, n := range names {
		b.WriteString("      - name: " + n[1] + "\n")
		b.WriteString("        in: path\n")
		b.WriteString("        required: true\n")
		b.WriteString("        schema:\n")
		b.WriteString("          type: string\n")
	}
}

func writeOperation(b *strings.Builder, path string, op operation) {
	b.WriteString("    " + op.method + ":\n")
	b.WriteString("      operationId: " + operationID(op.method, path) + "\n")
	if adminRoutes[op.pattern] {
		b.WriteString("      x-required-role: admin\n")
	}
	switch {
	case op.public:
		b.WriteString("      security: []\n")
	case basicAuthRoutes[op.pattern]:
		b.WriteString("      security:\n")
		b.WriteString("        - appPassword: []\n")
	default:
		b.WriteString("      security:\n")
		b.WriteString("        - sessionCookie: []\n")
		b.WriteString("        - bearerToken: []\n")
	}
	b.WriteString("      responses:\n")
	b.WriteString("        \"default\":\n")
	b.WriteString("          description: |\n")
	b.WriteString("            See docs/api-contract.md for this endpoint's request and response\n")
	b.WriteString("            bodies. Every failure uses the Error schema below.\n")
	b.WriteString("          content:\n")
	b.WriteString("            application/json:\n")
	b.WriteString("              schema:\n")
	b.WriteString("                $ref: \"#/components/schemas/Error\"\n")
}

// operationID builds a camelCase identifier from the method and path. Hyphens
// are folded into the camel casing rather than kept, so the result is a valid
// identifier in the languages that generate clients from this file.
func operationID(method, path string) string {
	id := method
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		for _, word := range strings.Split(strings.Trim(seg, "{}"), "-") {
			if word == "" {
				continue
			}
			id += strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return id
}

func methodRank(m string) int {
	for i, want := range methodOrder {
		if m == want {
			return i
		}
	}
	return len(methodOrder)
}

// yamlKey quotes a scalar so a path never has to be trusted as bare YAML.
func yamlKey(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// firstDifference reports where two versions of the spec diverge, so a failure
// names the route that changed instead of dumping two whole files.
func firstDifference(committed, generated string) string {
	c := strings.Split(committed, "\n")
	g := strings.Split(generated, "\n")
	for i := 0; i < len(c) && i < len(g); i++ {
		if c[i] != g[i] {
			return fmt.Sprintf("first difference at line %d:\n  committed: %s\n  generated: %s", i+1, c[i], g[i])
		}
	}
	return fmt.Sprintf("identical for %d lines, then the files differ in length (committed %d lines, generated %d)",
		min(len(c), len(g)), len(c), len(g))
}

const specPreamble = `# GENERATED FILE — DO NOT EDIT BY HAND.
#
# Regenerate with:
#
#     go test ./internal/httpapi -run TestOpenAPISpecMatchesRegisteredRoutes -update-openapi
#
# from server/. A contract test fails when this file and the server's route
# table disagree, so it is always an accurate answer to "does this endpoint
# exist, and does it need a session?"
openapi: 3.1.0
info:
  title: private-cloud API
  version: v1
  description: |
    The endpoints this server actually serves, generated from its route table.

    This file is deliberately narrow. It answers existence, method and auth
    posture — the things a client blocks on and the things that can be derived
    from the server itself. It does NOT describe request and response bodies:
    those live in docs/api-contract.md, which is written by hand and is the
    source of truth for what an endpoint means.

    The two documents have different jobs, and the difference matters. The
    contract also specifies endpoints that are only PROPOSED (Phases 5-9), so a
    client cannot use it to tell what is implemented. This file contains no
    proposed endpoints by construction. If it is not here, it is not served.

    Auth posture is derived by probing each route without credentials, not
    declared, so it cannot go stale when a handler's wrapper changes.
servers:
  - url: /
    description: Same origin as the web app — WebAuthn binds passkeys to an origin.
`

const specComponents = `
components:
  securitySchemes:
    sessionCookie:
      type: apiKey
      in: cookie
      name: pc_session
      description: Set by the WebAuthn, recovery-code and OIDC login flows.
    bearerToken:
      type: http
      scheme: bearer
      description: |
        A device token from POST /api/v1/auth/token. Used by pcsync and other
        headless clients.
    appPassword:
      type: http
      scheme: basic
      description: |
        A username and an app password, which is shown once at creation and
        stored argon2id-hashed. Accepted ONLY at POST /api/v1/auth/token and
        over /dav — an app password cannot reach the rest of the JSON API, so a
        leaked one cannot enrol a passkey or take the account.
  schemas:
    Error:
      type: object
      required: [error]
      description: |
        The shape of EVERY failure from every endpoint. Nested, not flat: a
        client that reads "error" as a string will get an object and fail to
        parse every error path. See the correction note in docs/api-contract.md.
      properties:
        error:
          type: object
          required: [code, message]
          properties:
            code:
              type: string
              description: Stable and safe to branch on.
              examples: [not_found, unauthenticated, rate_limited]
            message:
              type: string
              description: Human text. May be reworded; do not branch on it.
            request_id:
              type: string
              description: Echoed in the X-Request-Id header.
    Node:
      type: object
      description: |
        A file or folder, returned wherever either appears. Absent fields are
        omitted rather than null. Storage detail (blob keys, internal version
        ids) is deliberately not exposed.
      required: [id, kind, name, path, created_at, updated_at]
      properties:
        id: {type: string, format: uuid}
        kind: {type: string, enum: [file, folder]}
        name: {type: string}
        path: {type: string}
        parent_id:
          type: string
          format: uuid
          description: Absent on the root.
        created_at: {type: string, format: date-time}
        updated_at: {type: string, format: date-time}
        size:
          type: integer
          description: Files only.
        mime:
          type: string
          description: Files only.
        sha256:
          type: string
          description: |
            Whole-blob files only. A node carries sha256 XOR blake3, never both —
            the key names the algorithm, and a client verifying a download must
            run the one it was given.
        blake3:
          type: string
          description: Chunked (manifest-backed) files only. See sha256.
        trashed_at:
          type: string
          format: date-time
          description: Present only on trashed nodes.
`
