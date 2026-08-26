module github.com/guru-bharadwaj20/private-cloud/client

// The sync client is its OWN module, not a package under server/. It ships to
// laptops and desktops, so it must build with a pure-Go toolchain (no CGO) and
// must not drag in the server's pgx, blob store or WebAuthn dependencies. The
// only thing it shares with the server is the wire protocol — and a protocol is
// a contract, not shared code, so the FastCDC + BLAKE3 chunking parameters are
// re-declared here to match rather than imported across the module boundary.
go 1.25.0

require (
	fyne.io/systray v1.12.2
	github.com/fsnotify/fsnotify v1.10.1
	github.com/klauspost/compress v1.19.2
	github.com/tigerwill90/fastcdc v1.2.2
	github.com/zeebo/blake3 v0.2.4
	modernc.org/sqlite v1.56.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/cpuid/v2 v2.0.12 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
