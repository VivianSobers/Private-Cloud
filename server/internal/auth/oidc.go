package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC single sign-on: an ADDITIONAL way in, alongside passkeys, for onboarding
// people who already have a company identity provider. It never weakens the
// existing paths — passkey and recovery are unchanged, the admin is still
// bootstrapped by a passkey, and an OIDC login yields an ordinary web session.
//
// The token verification (JWKS fetch, signature, iss/aud/exp) is delegated to the
// vetted go-oidc library rather than hand-rolled: getting JWT validation subtly
// wrong is exactly how an SSO integration becomes an authentication bypass.

var (
	// ErrOIDCNoEmail rejects a token with no email — provisioning needs one.
	ErrOIDCNoEmail = errors.New("oidc token has no email claim")
	// ErrOIDCEmailUnverified rejects an unverified email. Provisioning on one would
	// let anyone who can set an unverified address at the IdP claim it here.
	ErrOIDCEmailUnverified = errors.New("oidc email is not verified")
	// ErrOIDCDomainNotAllowed rejects an email outside the configured domains.
	ErrOIDCDomainNotAllowed = errors.New("oidc email domain is not allowed")
	// ErrOIDCNonceMismatch rejects a token whose nonce does not match the one this
	// login started with — the replay/CSRF defence on the token itself.
	ErrOIDCNonceMismatch = errors.New("oidc nonce mismatch")
)

// OIDCConfig configures a provider.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// AllowedDomains, when non-empty, restricts sign-in to these email domains.
	AllowedDomains []string
}

// OIDCClaims is the verified subset of an ID token this system uses.
type OIDCClaims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// OIDCProvider wraps the OAuth2/OIDC flow for one configured provider.
type OIDCProvider struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	allowed  []string
}

// NewOIDCProvider performs OIDC discovery against the issuer and builds the flow.
// A network call — a failure here means the provider is unreachable or
// misconfigured, and the caller decides whether that disables SSO or aborts.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	return &OIDCProvider{
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		allowed:  cfg.AllowedDomains,
	}, nil
}

// AllowedDomains returns the configured domain allowlist.
func (p *OIDCProvider) AllowedDomains() []string { return p.allowed }

// AuthCodeURL builds the redirect that starts the flow: it carries the state (CSRF
// binding), the nonce (token replay binding), and the PKCE challenge derived from
// verifier (code-interception defence). GenerateVerifier/GenerateState live in the
// caller so it can stash them for the callback.
func (p *OIDCProvider) AuthCodeURL(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier))
}

// Exchange trades an authorization code for a verified ID token's claims. It
// verifies the token (signature, issuer, audience, expiry) via go-oidc and checks
// the nonce itself, so a token minted for a different login is rejected.
func (p *OIDCProvider) Exchange(ctx context.Context, code, verifier, nonce string) (OIDCClaims, error) {
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("oidc code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return OIDCClaims{}, errors.New("oidc response has no id_token")
	}
	idToken, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return OIDCClaims{}, fmt.Errorf("oidc verify: %w", err)
	}
	if idToken.Nonce != nonce {
		return OIDCClaims{}, ErrOIDCNonceMismatch
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return OIDCClaims{}, fmt.Errorf("oidc claims: %w", err)
	}
	return OIDCClaims{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
	}, nil
}

// LoginOIDC resolves verified claims to a session-ready user, provisioning one on
// first sign-in. It enforces the policy gates — email present, verified, and in an
// allowed domain — before any account is touched.
func (s *Service) LoginOIDC(ctx context.Context, claims OIDCClaims, allowedDomains []string) (*User, error) {
	if claims.Email == "" {
		return nil, ErrOIDCNoEmail
	}
	if !claims.EmailVerified {
		return nil, ErrOIDCEmailUnverified
	}
	if !domainAllowed(claims.Email, allowedDomains) {
		return nil, ErrOIDCDomainNotAllowed
	}

	u, err := s.store.FindUserByOIDC(ctx, claims.Issuer, claims.Subject)
	if err == nil {
		if u.Disabled() {
			return nil, ErrUserDisabled
		}
		return u, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}

	// First sign-in for this identity: provision a user.
	u, err = s.store.ProvisionOIDCUser(ctx, claims.Issuer, claims.Subject, claims.Email, claims.Name)
	if errors.Is(err, ErrUserExists) {
		// Two logins for the same new identity raced; the other won, so re-resolve.
		return s.store.FindUserByOIDC(ctx, claims.Issuer, claims.Subject)
	}
	return u, err
}

// domainAllowed reports whether an email is permitted. An empty allowlist permits
// any domain — the deployment chose not to restrict.
func domainAllowed(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(strings.ToLower(email), "@")
	if !ok {
		return false
	}
	for _, a := range allowed {
		if domain == strings.ToLower(strings.TrimSpace(a)) {
			return true
		}
	}
	return false
}
