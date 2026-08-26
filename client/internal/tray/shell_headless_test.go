//go:build !tray

package tray

import (
	"context"
	"errors"
	"testing"
)

// The default build has no tray, and says so with the sentinel the CLI turns
// into a fallback rather than a failure. This test is the guard on the property
// that made the tag worth having: a `pcsync` built without it links no GUI code
// and behaves exactly as it did before.
func TestHeadlessShellIsUnsupported(t *testing.T) {
	if Supported {
		t.Fatal("Supported must be false in a build without the tray tag")
	}
	err := NewShell().Run(context.Background(), NewModel(&fakeCtl{}), Options{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Run = %v, want ErrUnsupported", err)
	}
}
