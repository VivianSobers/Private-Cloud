package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
)

// OIDC single sign-on endpoints. Present only when a provider is configured; the
// existing passkey and recovery paths are untouched either way.

const (
	oidcFlowCookie = "pc_oidc_flow"
	// postLoginURL is where a successful SSO login lands. Hardcoded and
	// same-origin: never a value from the request, so the callback cannot be
	// turned into an open redirect.
	postLoginURL = "/"
)

// SetOIDC enables SSO by wiring a configured provider. Left unset otherwise, in
// which case the endpoints report the feature disabled.
func (s *Server) SetOIDC(p *auth.OIDCProvider) { s.oidc = p }

// oidcFlow is the transient per-login state, carried in a short-lived cookie
// between the login redirect and the callback.
type oidcFlow struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
}

func randToken() string {
	b := make([]byte, 32)
	// crypto/rand.Read never returns an error; since Go 1.24 it panics rather
	// than handing back weak bytes, which is the correct failure for a CSRF token.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// newFlowKey mints the HMAC key for the login-flow cookie.
func newFlowKey() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

// handleOIDCLogin starts the flow: it mints state (CSRF), nonce (token replay)
// and a PKCE verifier, stashes them in a cookie, and redirects to the provider.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, r, http.StatusNotFound, "oidc_disabled", "single sign-on is not enabled")
		return
	}

	flow := oidcFlow{State: randToken(), Nonce: randToken(), Verifier: oauth2.GenerateVerifier()}

	http.SetCookie(w, &http.Cookie{
		Name:     oidcFlowCookie,
		Value:    s.sealOIDCFlow(flow),
		Path:     "/api/v1/auth/oidc",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode, // Lax so the provider's redirect back carries it
		MaxAge:   600,
	})
	http.Redirect(w, r, s.oidc.AuthCodeURL(flow.State, flow.Nonce, flow.Verifier), http.StatusFound)
}

// handleOIDCCallback completes the flow: validate state, exchange the code, verify
// the token, resolve or provision the user, and issue a web session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeError(w, r, http.StatusNotFound, "oidc_disabled", "single sign-on is not enabled")
		return
	}
	// Always clear the flow cookie: it is single-use.
	s.clearCookie(w, oidcFlowCookie)

	// The provider signals user denial or an upstream error here rather than a code.
	if e := r.URL.Query().Get("error"); e != "" {
		s.log.Warn("oidc provider returned an error", "error", e)
		s.redirectSSOError(w, r)
		return
	}

	flow, ok := s.readOIDCFlow(r)
	if !ok {
		s.redirectSSOError(w, r)
		return
	}
	// State from the cookie must match the one echoed in the query — the CSRF check.
	if subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		s.log.Warn("oidc state mismatch", "remote", clientIP(r))
		s.redirectSSOError(w, r)
		return
	}

	claims, err := s.oidc.Exchange(r.Context(), r.URL.Query().Get("code"), flow.Verifier, flow.Nonce)
	if err != nil {
		s.log.Warn("oidc exchange failed", "error", err, "request_id", RequestID(r.Context()))
		s.redirectSSOError(w, r)
		return
	}

	user, err := s.auth.LoginOIDC(r.Context(), claims, s.oidc.AllowedDomains())
	if err != nil {
		// Policy rejections (unverified email, disallowed domain, disabled account)
		// and provisioning failures are uniform to the browser; the log has detail.
		s.log.Warn("oidc login rejected", "error", err, "subject", claims.Subject)
		s.redirectSSOError(w, r)
		return
	}

	token, expires, err := s.auth.NewSessionToken(r.Context(), user, auth.SessionKindWeb, r.UserAgent())
	if err != nil {
		s.serverError(w, r, "oidc session", err)
		return
	}
	s.setSessionCookie(w, token, expires)
	s.log.Info("oidc login", "user", user.Username)
	http.Redirect(w, r, postLoginURL, http.StatusFound)
}

// sealOIDCFlow encodes the flow and authenticates it with an HMAC.
//
// The state/query comparison in the callback is a CSRF defence only if the
// cookie side cannot be chosen by the attacker. Unsigned, it was just base64
// JSON: anyone able to set a cookie on the victim's browser — a sibling
// subdomain, a network position on plain HTTP, a stray XSS — could plant a flow
// whose state matches a login THEY started, and silently sign the victim in as
// the attacker. Everything the victim then files goes into the attacker's
// account.
//
// Signing makes the cookie unforgeable without the key, so the state check
// compares two values that both originate here.
func (s *Server) sealOIDCFlow(flow oidcFlow) string {
	blob, _ := json.Marshal(flow)
	mac := hmac.New(sha256.New, s.oidcFlowKey)
	mac.Write(blob)
	return base64.RawURLEncoding.EncodeToString(blob) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// readOIDCFlow decodes the flow cookie and verifies its signature.
func (s *Server) readOIDCFlow(r *http.Request) (oidcFlow, bool) {
	c, err := r.Cookie(oidcFlowCookie)
	if err != nil {
		return oidcFlow{}, false
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return oidcFlow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return oidcFlow{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return oidcFlow{}, false
	}
	mac := hmac.New(sha256.New, s.oidcFlowKey)
	mac.Write(raw)
	// Constant time, like every other secret comparison here: the signature is
	// checked before the contents are trusted at all.
	if !hmac.Equal(got, mac.Sum(nil)) {
		return oidcFlow{}, false
	}

	var flow oidcFlow
	if err := json.Unmarshal(raw, &flow); err != nil {
		return oidcFlow{}, false
	}
	if flow.State == "" || flow.Nonce == "" || flow.Verifier == "" {
		return oidcFlow{}, false
	}
	return flow, true
}

// redirectSSOError sends the browser back to the app with a failure marker, so a
// failed SSO attempt lands on a page that can explain it rather than on raw JSON.
func (s *Server) redirectSSOError(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, postLoginURL+"?sso_error=1", http.StatusFound)
}
