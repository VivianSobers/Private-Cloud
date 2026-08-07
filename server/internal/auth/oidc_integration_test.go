package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
)

func oidcFixture(t *testing.T) *auth.Service {
	t.Helper()
	dsn := os.Getenv("PC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PC_TEST_DATABASE_URL not set; skipping OIDC integration tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := db.Open(ctx, dsn, 8, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Migrate(ctx, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	svc, err := auth.NewService(auth.NewStore(database.Pool), auth.Config{
		RPDisplayName: "Test", RPID: "localhost",
		RPOrigins: []string{"http://localhost"}, SessionTTL: time.Hour,
	}, log)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	return svc
}

func verifiedClaims(subject, email string) auth.OIDCClaims {
	return auth.OIDCClaims{
		Issuer: "https://idp.example.com", Subject: subject,
		Email: email, EmailVerified: true, Name: "Test User",
	}
}

// A first SSO login provisions a user; a second for the same identity resolves to
// the same account rather than a duplicate.
func TestOIDCProvisionsThenResolves(t *testing.T) {
	svc := oidcFixture(t)
	ctx := context.Background()

	claims := verifiedClaims("subject-"+uniqueSuffix(), "person-"+uniqueSuffix()+"@example.com")
	u1, err := svc.LoginOIDC(ctx, claims, nil)
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if u1.IsAdmin {
		t.Error("OIDC user must not be admin")
	}

	u2, err := svc.LoginOIDC(ctx, claims, nil)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if u1.ID != u2.ID {
		t.Errorf("second login created a new user: %s != %s", u1.ID, u2.ID)
	}
}

// Two identities whose emails share a local part get distinct usernames.
func TestOIDCUsernameDedup(t *testing.T) {
	svc := oidcFixture(t)
	ctx := context.Background()

	local := "dup" + uniqueSuffix()
	a, err := svc.LoginOIDC(ctx, verifiedClaims("sa-"+uniqueSuffix(), local+"@a.example.com"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.LoginOIDC(ctx, verifiedClaims("sb-"+uniqueSuffix(), local+"@b.example.com"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Username == b.Username {
		t.Errorf("usernames collided: both %q", a.Username)
	}
}

// The policy gates reject before any account is created.
func TestOIDCPolicyGates(t *testing.T) {
	svc := oidcFixture(t)
	ctx := context.Background()

	unverified := verifiedClaims("s-"+uniqueSuffix(), "x"+uniqueSuffix()+"@example.com")
	unverified.EmailVerified = false
	if _, err := svc.LoginOIDC(ctx, unverified, nil); !errors.Is(err, auth.ErrOIDCEmailUnverified) {
		t.Errorf("unverified email: err = %v, want ErrOIDCEmailUnverified", err)
	}

	noEmail := verifiedClaims("s2-"+uniqueSuffix(), "")
	if _, err := svc.LoginOIDC(ctx, noEmail, nil); !errors.Is(err, auth.ErrOIDCNoEmail) {
		t.Errorf("missing email: err = %v, want ErrOIDCNoEmail", err)
	}

	wrongDomain := verifiedClaims("s3-"+uniqueSuffix(), "y"+uniqueSuffix()+"@evil.com")
	if _, err := svc.LoginOIDC(ctx, wrongDomain, []string{"example.com"}); !errors.Is(err, auth.ErrOIDCDomainNotAllowed) {
		t.Errorf("disallowed domain: err = %v, want ErrOIDCDomainNotAllowed", err)
	}
}

// A disabled account cannot sign in via SSO, just as it cannot via a passkey.
func TestOIDCDisabledUserRefused(t *testing.T) {
	svc := oidcFixture(t)
	ctx := context.Background()

	claims := verifiedClaims("sd-"+uniqueSuffix(), "d"+uniqueSuffix()+"@example.com")
	u, err := svc.LoginOIDC(ctx, claims, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store().SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoginOIDC(ctx, claims, nil); !errors.Is(err, auth.ErrUserDisabled) {
		t.Errorf("disabled user: err = %v, want ErrUserDisabled", err)
	}
}

func uniqueSuffix() string {
	return uuid.NewString()[:8]
}
