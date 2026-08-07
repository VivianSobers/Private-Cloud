package auth

import "testing"

func TestUsernameFromEmail(t *testing.T) {
	cases := map[string]string{
		"vivian@example.com":   "vivian",
		"First.Last@corp.co":   "first.last",
		"weird+tag@x.io":       "weirdtag",
		"UPPER@x.io":           "upper",
		"...@x.io":             "user", // degenerate -> fallback
		"a_b-c@x.io":           "a_b-c",
		"has spaces here@x.io": "hasspaceshere",
		"@nodomainlocalpart":   "user",
	}
	for email, want := range cases {
		if got := usernameFromEmail(email); got != want {
			t.Errorf("usernameFromEmail(%q) = %q, want %q", email, got, want)
		}
	}
}

func TestDomainAllowed(t *testing.T) {
	// Empty allowlist permits anything.
	if !domainAllowed("a@anywhere.com", nil) {
		t.Error("empty allowlist should permit any domain")
	}
	allowed := []string{"example.com", "Corp.CO"}
	if !domainAllowed("v@example.com", allowed) {
		t.Error("listed domain rejected")
	}
	if !domainAllowed("v@corp.co", allowed) {
		t.Error("domain match should be case-insensitive")
	}
	if domainAllowed("v@evil.com", allowed) {
		t.Error("unlisted domain permitted")
	}
	if domainAllowed("not-an-email", allowed) {
		t.Error("value with no domain permitted")
	}
}
