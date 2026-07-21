package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"golang.org/x/net/webdav"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/webdavfs"
)

// davPrefix is where the tree is mounted. Kept off /api on purpose: WebDAV is
// a different protocol with a different auth scheme, and mixing the two under
// one prefix would make it far too easy for a future route to inherit Basic
// auth by accident.
const davPrefix = "/dav"

// davHandler builds the WebDAV endpoint.
//
// The lock system is shared across requests and lives in memory. WebDAV locks
// exist to stop two clients writing the same file at once; an in-memory system
// is correct for a single-node deployment and loses locks on restart, which is
// the same thing that happens when a client's connection drops. A
// database-backed lock table would be needed for multiple replicas, and that
// is a Phase 3 problem.
func (s *Server) davHandler() http.Handler {
	locks := webdav.NewMemLS()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := CurrentUser(r.Context())
		if user == nil {
			s.requireBasicAuth(w)
			return
		}

		// The root folder is created lazily, and a user who has only ever used
		// WebDAV has never hit the JSON API path that creates it. Without this,
		// their very first PROPFIND resolves "/" to nothing and the mount fails
		// with a 404 that looks like the server is broken.
		if _, err := s.files.Store().EnsureRoot(r.Context(), user.ID); err != nil {
			s.log.Error("ensure root for webdav", "error", err, "user", user.Username)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		// The FileSystem is built per request and closes over the owner, so
		// there is no code path that could address another user's files —
		// ownership is a field here, not an argument a handler might forget.
		h := &webdav.Handler{
			Prefix:     davPrefix,
			FileSystem: webdavfs.New(s.files, user.ID, s.log),
			LockSystem: locks,
			Logger: func(req *http.Request, err error) {
				if err == nil {
					return
				}
				// Missing files are routine during discovery: Finder and
				// Explorer probe for .DS_Store, desktop.ini and friends on
				// every directory listing. Logging those at warn would bury
				// everything that matters.
				if errors.Is(err, webdavfs.ErrNotExist()) {
					return
				}
				s.log.Warn("webdav",
					"method", req.Method,
					"path", req.URL.Path,
					"user", user.Username,
					"error", err,
					"request_id", RequestID(req.Context()))
			},
		}
		h.ServeHTTP(w, r)
	})
}

// requireBasicAuth challenges the client.
//
// The realm is spelled out because it is the only text a mounting client shows
// the user, and "enter your app password" is far more useful than the account
// password they are about to try and fail with.
func (s *Server) requireBasicAuth(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="private cloud — use an app password", charset="UTF-8"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("app password required\n"))
}

// withDavAuth authenticates a WebDAV request.
//
// Two credentials are accepted, in this order:
//
//   - An app password via Basic auth. This is the path that matters: a
//     filesystem driver cannot run a WebAuthn ceremony, so passkeys are simply
//     not available to it.
//   - An ordinary session cookie, so the web UI could use the same endpoint
//     without minting a second credential.
//
// A recovery session is refused outright. Its entire purpose is enrolling a
// passkey, and letting it reach files would make a printed code equivalent to
// one.
func (s *Server) withDavAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rate limited like the auth endpoints. Basic auth over WebDAV is the
		// one place in this system where an online guessing attack against a
		// secret is possible at all.
		if !s.authLimiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		if username, password, ok := r.BasicAuth(); ok {
			user, err := s.auth.Store().VerifyAppPassword(r.Context(), password)
			if err != nil {
				s.log.Warn("webdav auth failed",
					"username", username, "error", err, "remote", clientIP(r))
				s.requireBasicAuth(w)
				return
			}
			// The username is not used for lookup — the app password identifies
			// its own account — but it IS checked, because a mount configured
			// with one person's username and another's password should fail
			// loudly rather than silently operating on the wrong tree.
			//
			// Empty is allowed: some clients omit it entirely, and refusing
			// those would break a mount for no security gain.
			if username != "" && !strings.EqualFold(username, user.Username) {
				s.log.Warn("webdav username does not match the app password's account",
					"supplied", username, "actual", user.Username)
				s.requireBasicAuth(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(withUser(r.Context(), user)))
			return
		}

		sess, user, err := s.auth.Authenticate(r.Context(), sessionTokenFrom(r, s.cookieName))
		if err != nil || sess.Kind == auth.SessionKindRecovery {
			s.requireBasicAuth(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), user)))
	})
}
