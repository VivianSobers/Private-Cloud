package httpapi

import (
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
// docs/status.md ("The seam: who owns what") calls contract tests "the shared
// safety net … the only place both tracks' assumptions meet, and it fails
// loudly when they drift."
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

// TestWebClientCallsOnlyRoutesThatExist closes the loop the whole contract test
// was written for.
//
// The original failure was not that a spec drifted — it was that `web/` shipped
// entire views calling Phase 5, 7 and 8 endpoints that had never been built, and
// nothing in the repository disagreed. An earlier version of this test pinned
// those endpoints as *expected to 404*, which was right while they were
// unimplemented. They are all implemented now, so that list is empty and the
// check has been turned around to assert the property that actually matters:
//
//	every /api/v1 path the web client calls is a route this server serves.
//
// This fails the moment somebody adds a client call for an endpoint that does
// not exist, which is the drift that took three phases to notice.
func TestWebClientCallsOnlyRoutesThatExist(t *testing.T) {
	source, err := os.ReadFile(filepath.FromSlash("../../../web/src/api.ts"))
	if err != nil {
		t.Skipf("web client not present: %v", err)
	}

	registered := map[string]bool{}
	for _, pattern := range newContractServer(t).Routes() {
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			continue
		}
		registered[normalisePath(path)] = true
	}

	calls := apiPathsIn(string(source))
	// A floor, so a change to api.ts's style that stopped the extraction working
	// fails loudly instead of silently checking nothing.
	if len(calls) < 20 {
		t.Fatalf("extracted only %d API calls from web/src/api.ts — the extraction "+
			"has probably stopped matching the file's style, which would make this "+
			"test pass while checking nothing", len(calls))
	}

	for _, call := range calls {
		if !registered[normalisePath(call)] {
			t.Errorf("web/src/api.ts calls %s, which this server does not serve.\n"+
				"Either implement the route or remove the client call — a view built "+
				"against an endpoint that does not exist is the exact drift this test "+
				"exists to catch.", call)
		}
	}
}

// apiPathsIn extracts the distinct /api/v1 paths a source file calls, whether
// they are written as TypeScript template literals or as Go string
// concatenation.
//
// One extractor for both directions of the contract check. There were two, one
// per test, differing only in which quote characters they recognised — and two
// implementations of "which endpoints does this client call" is precisely the
// duplication that ends with the two tests disagreeing about the same file.
func apiPathsIn(source string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range apiPathRe.FindAllStringSubmatch(source, -1) {
		path := strings.TrimRight(pathPart(m[1]), "/")
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// apiPathRe matches an /api/v1 path opened by a backtick or a double quote.
var apiPathRe = regexp.MustCompile("[`\"](/api/v1[^`\"]*)")

// pathPart trims a captured template literal down to its PATH.
//
// Two things have to be got right, and both were wrong in the first version of
// this test — which is worth recording, because a contract test that reports
// false positives gets muted and then protects nothing.
//
//  1. A '?' only starts a query string when it is OUTSIDE an interpolation. The
//     client writes suffixes like `${q ? "?" + q : ""}`, where the '?' is a
//     ternary inside ${...} and cutting there leaves a mangled path.
//  2. A capture may END inside an interpolation, because a nested template
//     literal closes the outer match early. That trailing partial `${…` is a
//     query builder, not a path segment, so it is dropped.
//
// The rule that separates the two cases: an interpolation is a path PARAMETER
// only when it is preceded by '/', i.e. it forms a whole segment
// (`/nodes/${id}`). One appended to the end of a segment
// (`/content${download ? "?download=1" : ""}`) is building a query suffix, and
// everything from there on is not part of the path.
func pathPart(s string) string {
	depth := 0
	lastOpen := -1
	for i := 0; i < len(s); i++ {
		switch {
		case depth == 0 && s[i] == '$' && i+1 < len(s) && s[i+1] == '{':
			if i == 0 || s[i-1] != '/' {
				return s[:i]
			}
			depth++
			lastOpen = i
			i++
		case depth > 0 && s[i] == '{':
			depth++
		case depth > 0 && s[i] == '}':
			depth--
		case depth == 0 && s[i] == '?':
			// A real query string starts here.
			return s[:i]
		}
	}
	if depth > 0 && lastOpen >= 0 {
		// The literal ended mid-interpolation: keep only the path before it.
		return s[:lastOpen]
	}
	return s
}

// interpolationRe matches both the client's ${...} and the mux's {...}.
var interpolationRe = regexp.MustCompile(`\$\{[^}]*\}|\{[^}]*\}`)

// normalisePath reduces a path to its shape, so a client's `/nodes/${id}/tags`
// and a route's `/nodes/{id}/tags` compare equal.
func normalisePath(p string) string {
	return interpolationRe.ReplaceAllString(strings.TrimRight(p, "/"), "*")
}

// awaitingClient are routes this server serves that no client in this repository
// calls yet, each with the reason.
//
// Being here is not a defect. The split of work means the two halves land at
// different times, and a server route arriving before its UI is the normal
// direction — the reverse is the one that hurts. What IS a defect is a route
// sitting unconsumed with nobody able to say whether that is deliberate, which is
// how a feature comes to be finished, tested, documented and forgotten.
//
// So the list is a declaration with an owner and a reason, and
// TestEveryRouteIsConsumedOrDeclaredPending fails both ways: an undeclared
// unconsumed route, and a declaration for a route that is now called or no longer
// exists. Deleting a line from here is part of shipping the client for it.
var awaitingClient = map[string]string{
	// The list was thirteen shapes when this test was written: five device routes,
	// the whole people browser, /similar, /chat, /admin/storage and the admin
	// session routes. Every one of them now has a client, and each was deleted
	// from here as its UI landed — which is the shape a healthy declaration list
	// has, and it is now empty.
	//
	// The last to go was /devices/*/push, which sat here longest because it was
	// not waiting on a UI at all: PushManager.subscribe needs a VAPID public key
	// the server did not publish, and nothing would have delivered a notification
	// if it had. Both halves were behind the API. GET /push/key and internal/push
	// closed them, and web/src/push.ts subscribes.
	//
	// An empty map is the desired state, not a sign the test stopped working —
	// TestEveryRouteIsConsumedOrDeclaredPending still fails on any new route that
	// ships without a client or a reason.
}

// invisiblyConsumed are routes a client DOES call, in a way this test cannot
// see. They are declared rather than silently exempted, because "no client calls
// this" and "the extractor cannot follow this call" are completely different
// facts and only one of them is a reason to be worried.
//
// Two shapes defeat static extraction. pcsync builds paths by concatenation —
// "/api/v1/chunks/" + hash — so only the prefix is a literal and the parameter
// segment is invisible. And tus-js-client is handed an endpoint and derives every
// subsequent URL from the Location header, so the per-upload path appears in no
// source file at all.
var invisiblyConsumed = map[string]string{
	"/api/v1/uploads/*":        "tus; the client follows the Location header",
	"/api/v1/chunks/*":         "pcsync; path built by concatenation",
	"/api/v1/nodes/*/manifest": "pcsync; path built by concatenation",
	"/api/v1/auth/oidc/login":  "SSO; a plain link in SignIn.tsx, not a fetch",
	// The provider redirects the browser here; nothing in this repository calls it.
	"/api/v1/auth/oidc/callback": "SSO; the identity provider redirects here",
}

// operationalRoutes are not part of the client API surface at all.
var operationalRoutes = map[string]bool{
	"/healthz": true, "/readyz": true, "/metrics": true,
	"/api/v1/version": true,
	"/api/":           true,
	davPrefix:         true, davPrefix + "/": true,
}

// TestEveryRouteIsConsumedOrDeclaredPending is TestWebClientCallsOnlyRoutesThatExist
// pointed the other way.
//
// That test catches a client calling an endpoint that does not exist, which was
// the original drift. This one catches the drift that replaced it: the audit
// found roughly thirty routes built, tested and documented that no client calls
// — five device endpoints, the whole people browser, /chat, /similar,
// /admin/storage — and nothing anywhere recorded whether that was a plan or an
// oversight.
//
// It cannot tell those apart on its own, and does not try. It requires somebody
// to have written down which, and fails when a route is neither consumed nor
// declared. It also fails on a stale declaration, so the list shrinks as clients
// ship rather than becoming a graveyard nobody prunes.
//
// That mechanism has now been exercised: of the thirteen shapes first declared,
// twelve have shipped a client and been deleted from the list, and the deletions
// were forced by this test rather than remembered by anybody.
func TestEveryRouteIsConsumedOrDeclaredPending(t *testing.T) {
	consumed := map[string]bool{}
	for _, src := range []struct{ path, desc string }{
		{"../../../web/src/api.ts", "web client"},
		{"../../../web/src/webauthn.ts", "web client (WebAuthn)"},
		{"../../../web/src/upload.ts", "web client (uploads)"},
		{"../../../client/internal/api/client.go", "sync client"},
	} {
		source, err := os.ReadFile(filepath.FromSlash(src.path))
		if err != nil {
			t.Skipf("%s not present: %v", src.desc, err)
		}
		for _, call := range apiPathsIn(string(source)) {
			consumed[normalisePath(call)] = true
		}
	}

	seen := map[string]bool{}
	for _, pattern := range newContractServer(t).Routes() {
		path := pattern
		if _, p, ok := strings.Cut(pattern, " "); ok {
			path = p
		}
		if operationalRoutes[path] {
			continue
		}
		shape := normalisePath(path)
		seen[shape] = true

		_, pending := awaitingClient[shape]
		_, invisible := invisiblyConsumed[shape]

		if consumed[shape] {
			if pending {
				t.Errorf("awaitingClient still lists %q, but a client calls it now — "+
					"delete the line, that is what shipping it looks like", shape)
			}
			if invisible {
				t.Errorf("invisiblyConsumed lists %q, but the extractor finds it "+
					"perfectly well — drop the exemption", shape)
			}
			continue
		}
		if !pending && !invisible {
			t.Errorf("%s is served and no client calls it, and nothing says why.\n"+
				"Add it to awaitingClient with a reason, or to invisiblyConsumed if a "+
				"client does call it in a way this test cannot see. A route nobody "+
				"calls and nobody has decided about is how a feature gets finished "+
				"and then forgotten.", shape)
		}
	}

	for _, decl := range []struct {
		name  string
		items map[string]string
	}{
		{"awaitingClient", awaitingClient},
		{"invisiblyConsumed", invisiblyConsumed},
	} {
		for shape := range decl.items {
			if !seen[shape] {
				t.Errorf("%s names %q, which this server does not serve — "+
					"drop the line or fix the shape", decl.name, shape)
			}
		}
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

// widenedQueryParams are the query parameters that CHANGE WHAT AN ENDPOINT
// MEANS rather than merely filtering it, together with the client files that
// have to opt into them.
//
// This exists because the route-level test above cannot see them. A parameter
// is not a route, so `?include_shared=true` sat in this repository for a phase
// and a half being served, documented, and called by nothing — and status.md
// carried it as an open item that nothing could ever fail on. The thirteen
// unconsumed route shapes were caught by a test; this one was caught by a
// person reading a table, which is exactly the failure mode the route test was
// written to end.
//
// The rule is narrow on purpose. An ordinary filter does not belong here: a
// `limit` nobody passes costs nothing. These are the parameters whose absence
// makes a working client silently show LESS than the user is entitled to see.
var widenedQueryParams = map[string]struct {
	why      string
	callers  []string
	minCalls int
}{
	"include_shared": {
		why: "widens children, search and tags to content the caller was granted " +
			"but does not own. Every view that lists nodes has to opt in, or a " +
			"grantee sees a folder in /shared and an empty listing inside it",
		callers: []string{
			"../../../web/src/api.ts",
			"../../../web/src/Browser.tsx",
			"../../../web/src/TagBrowser.tsx",
		},
		// api.ts sends it on children, search and tags: three call sites, and a
		// count rather than a boolean so that wiring one and forgetting the
		// other two fails here instead of at somebody's second click.
		minCalls: 3,
	},
}

// TestWidenedQueryParametersHaveCallers is the route test's missing half.
//
// It fails in both directions the route test does: a parameter the server
// accepts that no client sends, and — through minCalls — a parameter wired into
// one listing and forgotten in the others.
func TestWidenedQueryParametersHaveCallers(t *testing.T) {
	for param, spec := range widenedQueryParams {
		served := false
		for _, file := range serverSourceFiles(t) {
			if strings.Contains(file, `"`+param+`"`) {
				served = true
				break
			}
		}
		if !served {
			t.Errorf("widenedQueryParams lists %q but no handler reads it — "+
				"delete the entry, or the list becomes the graveyard it exists to prevent", param)
			continue
		}

		total := 0
		for _, path := range spec.callers {
			source, err := os.ReadFile(filepath.FromSlash(path))
			if err != nil {
				t.Skipf("%s not present: %v", path, err)
			}
			total += strings.Count(string(source), param)
		}
		if total < spec.minCalls {
			t.Errorf("?%s= is served and reached %d client mention(s), want at least %d.\n"+
				"Why it matters: %s\n"+
				"A parameter nobody sends is a feature that is finished on one side "+
				"and invisible on the other — which is how this one went a phase and "+
				"a half without a caller.",
				param, total, spec.minCalls, spec.why)
		}
	}
}

// serverSourceFiles reads the handler sources once, so the test above can ask
// whether the server actually reads a parameter rather than trusting the list.
func serverSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := filepath.Glob(filepath.FromSlash("*.go"))
	if err != nil {
		t.Fatalf("globbing handler sources: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e, "_test.go") {
			continue
		}
		b, err := os.ReadFile(e)
		if err != nil {
			t.Fatalf("reading %s: %v", e, err)
		}
		out = append(out, string(b))
	}
	return out
}
