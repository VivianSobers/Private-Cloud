//go:build !tray

package tray

import "context"

// Supported reports whether this build can draw a real tray. It is a build-time
// constant so a caller can offer the fallback before it has tried and failed.
const Supported = false

// headlessShell is the tray on a build made without the `tray` tag: there is no
// tray, and saying so immediately is the whole implementation. Nothing here
// imports the systray library, which is the point — the default `pcsync` binary
// links no GUI code, needs no cgo, and behaves exactly as it did before the tray
// existed.
type headlessShell struct{}

// NewShell returns the shell this build supports.
func NewShell() Shell { return headlessShell{} }

// Run refuses, with the sentinel the caller can act on.
func (headlessShell) Run(context.Context, *Model, Options) error { return ErrUnsupported }
