package auth_test

import (
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
)

// Everything ParseUserAgent returns is self-reported by the client, so these
// tests pin the two properties that matter: the more specific platform wins, and
// an agent that says nothing recognisable yields nothing rather than a guess.
func TestParseUserAgent(t *testing.T) {
	cases := []struct {
		name          string
		ua            string
		wantPlatform  string
		wantAppVerion string
	}{
		{"pcsync on linux", "pcsync/0.4.1 (linux)", "linux", "0.4.1"},
		{"pcsync on windows", "pcsync/1.0.0 (Windows NT 10.0)", "windows", "1.0.0"},
		{"macos", "pcsync/0.9 (Darwin 23.4.0)", "macos", "0.9"},
		{"ios", "PrivateCloud/2.1 (iPhone; iOS 17.2)", "ios", "2.1"},
		{
			// Android agents contain "Linux" too. The more specific answer is the
			// useful one, so the check order is load-bearing.
			"android beats linux",
			"PrivateCloud/2.1 (Linux; Android 14; Pixel 8)",
			"android", "2.1",
		},
		{"version without a platform", "Go-http-client/2.0", "", "2.0"},
		{"nothing recognisable", "????", "", ""},
		{"empty", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			platform, version := auth.ParseUserAgent(tc.ua)
			if platform != tc.wantPlatform {
				t.Errorf("platform = %q, want %q", platform, tc.wantPlatform)
			}
			if version != tc.wantAppVerion {
				t.Errorf("version = %q, want %q", version, tc.wantAppVerion)
			}
		})
	}
}
