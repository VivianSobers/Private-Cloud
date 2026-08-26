package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrUnsupported is what a build with no tray support returns from Run. It is a
// sentinel rather than a log line because the caller — `pcsync tray` — can do
// something better than print it: fall back to the text status line, which is
// exactly the behaviour this binary has always had.
var ErrUnsupported = errors.New("this pcsync build has no system tray support (rebuild with -tags tray)")

// Shell is the platform half of the tray, and the only interface between the
// model and an operating system. There are two implementations, chosen by build
// tag: a real one over fyne.io/systray behind `tray`, and a no-op otherwise.
//
// The tag exists because a system tray is not free to depend on. On macOS the
// library needs cgo and an Objective-C toolchain, and on Linux it needs a
// session bus; the default `pcsync` binary is a headless daemon that has to
// cross-compile to six targets from a build machine with a display on none of
// them. Keeping the tray behind a tag means the daemon everybody actually runs
// stays CGO-free and buildable anywhere, and the desktop build is the same
// source with one flag.
type Shell interface {
	// Run takes over the calling goroutine, drives the platform's tray until the
	// user quits it or ctx is cancelled, and returns. It must be called from the
	// main goroutine: every platform's tray belongs to the thread that created it.
	Run(ctx context.Context, m *Model, opts Options) error
}

// Options configures a shell run. The zero value is usable.
type Options struct {
	// Interval is how often the daemon is polled for a fresh status. Defaults to
	// DefaultInterval.
	Interval time.Duration
	// Log receives the errors an action produced. A tray has nowhere to put an
	// error dialog that a person would thank us for, so a failed click is logged
	// and the next poll shows the truth.
	Log *slog.Logger
	// Open hands a URL or a folder to the desktop. Injectable so a test can drive
	// the whole shell without launching a browser; defaults to OpenTarget.
	Open func(target string) error
}

// DefaultInterval matches `pcsync watch`: fast enough that a click's effect
// looks immediate, slow enough that an idle tray is not a background process
// waking the disk twice a second.
const DefaultInterval = 2 * time.Second

// withDefaults fills in the zero fields.
func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Open == nil {
		o.Open = OpenTarget
	}
	return o
}

// OpenTarget hands a URL or a directory to the desktop's own handler. The target
// comes from the daemon rather than from a person, but it still reaches a shell,
// so it is checked first: only an http(s) URL or an absolute path is passed on.
// That refusal is what keeps a compromised or confused daemon from turning this
// into a way to run a command.
func OpenTarget(target string) error {
	switch {
	case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"):
	case filepath.IsAbs(target):
	default:
		return fmt.Errorf("refusing to open %q: not an http(s) URL or an absolute path", target)
	}

	switch runtime.GOOS {
	case "windows":
		// rundll32's FileProtocolHandler takes the target as an ordinary argument,
		// so unlike `cmd /c start` there is no second round of shell quoting to get
		// wrong for a path containing a space or an ampersand.
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target).Start()
	case "darwin":
		return exec.Command("open", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}
