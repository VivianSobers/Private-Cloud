package tray

import (
	"embed"
	"fmt"
	"runtime"
)

//go:generate go run ./icons/generate.go -out ./icons

// icons holds one drawn icon per tray state, in both encodings a system tray
// asks for. They are embedded rather than installed beside the binary because
// `pcsync` ships as a single file people copy onto a laptop, and an icon that
// can go missing is a tray that can come up blank.
//
// The cost, stated plainly: about 24 KB of PNG and .ico rides in every build,
// including the headless daemon that will never draw them. That is the price of
// keeping the assets, their generator and their test in the default build where
// `go vet` and `go test` cover them, instead of behind the tag where nothing
// would notice a truncated file until somebody with a desktop tried to run it.
//
//go:embed icons/*.png icons/*.ico
var icons embed.FS

// Icon returns the icon bytes for a state in the encoding this platform's tray
// wants: Windows loads a real .ico and nothing else, while the macOS and Linux
// backends take a PNG.
func Icon(s State) []byte {
	if runtime.GOOS == "windows" {
		return IconICO(s)
	}
	return IconPNG(s)
}

// IconPNG returns the PNG form of a state's icon.
func IconPNG(s State) []byte { return read(s, "png") }

// IconICO returns the Windows .ico form of a state's icon.
func IconICO(s State) []byte { return read(s, "ico") }

// read pulls one asset out of the embedded set. A missing file can only be a
// build that embedded the wrong thing, so it panics: an icon silently absent is
// a tray that renders as an empty square with no explanation, and the failure
// happens the first time the tray runs, not in the hands of a user.
func read(s State, ext string) []byte {
	name := fmt.Sprintf("icons/%s.%s", s.String(), ext)
	data, err := icons.ReadFile(name)
	if err != nil {
		panic("tray: embedded icon missing: " + name)
	}
	return data
}
