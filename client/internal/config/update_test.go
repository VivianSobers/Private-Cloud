package config

import (
	"strconv"
	"testing"
	"time"
)

// base returns a minimal valid config with an "update" block spliced in, so each
// case below differs only in the thing it is testing.
func base(t *testing.T, updateBlock string) string {
	t.Helper()
	root := t.TempDir()
	body := `{
		"server_url": "https://cloud.example.ts.net",
		"username": "vivian",
		"app_password": "pcap_0011223344556677_00112233445566778899aabbccddeeff",
		"root": ` + strconv.Quote(root)
	if updateBlock != "" {
		body += ",\n\t\t\"update\": " + updateBlock
	}
	return write(t, body+"\n}")
}

func TestUpdaterIsOffUnlessAskedFor(t *testing.T) {
	// The default that matters most: a config with no update block whatsoever
	// must not leave a machine willing to replace its own binary.
	c, err := Load(base(t, ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Update.Enabled {
		t.Fatal("the updater is on in a config that never mentioned it")
	}
	// The rest of the block still gets its defaults, so opting in is one line.
	if c.Update.FeedURL != DefaultFeedURL || c.Update.Issuer != DefaultIssuer || c.Update.Identity != DefaultIdentity {
		t.Errorf("updater defaults not applied: %+v", c.Update)
	}
	if c.Update.CheckInterval() != 24*time.Hour {
		t.Errorf("check interval = %s, want 24h", c.Update.CheckInterval())
	}
	if c.Update.AllowDowngrade {
		t.Error("downgrades are permitted by default")
	}
}

func TestUpdaterOptIn(t *testing.T) {
	c, err := Load(base(t, `{"enabled": true, "check_hours": 6}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.Update.Enabled {
		t.Fatal("enabled was not read")
	}
	if c.Update.CheckInterval() != 6*time.Hour {
		t.Errorf("check interval = %s, want 6h", c.Update.CheckInterval())
	}
}

func TestUpdaterRejectsSettingsThatWouldWeakenTheCheck(t *testing.T) {
	cases := map[string]string{
		// An updater reading a plaintext feed is an install-anything button for
		// anyone on the path, so it is refused rather than warned about.
		"plaintext feed":   `{"enabled": true, "feed_url": "http://example.test/feed.json"}`,
		"not a URL at all": `{"enabled": true, "feed_url": "example.test/feed.json"}`,
		"broken identity":  `{"enabled": true, "identity": "([unclosed"}`,
	}
	for name, block := range cases {
		if _, err := Load(base(t, block)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestUpdaterIgnoresAHalfFilledBlockWhenDisabled(t *testing.T) {
	// A disabled updater with a leftover setting is not a misconfiguration —
	// nothing reads the feed, so nothing can be misled by it.
	if _, err := Load(base(t, `{"feed_url": "http://example.test/feed.json"}`)); err != nil {
		t.Fatalf("a disabled updater with a plaintext feed should load: %v", err)
	}
}
