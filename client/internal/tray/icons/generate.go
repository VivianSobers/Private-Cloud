//go:build ignore

// Command generate draws the tray icons and writes them next to itself, as both
// a PNG and a Windows .ico, one pair per tray state.
//
// The icons are generated rather than drawn by hand, and the generator — not the
// output — is the source you edit. Five states have to stay distinguishable at
// 16 logical pixels on a busy taskbar, and the way to keep them so is one set of
// shared primitives and one palette, not five files from a drawing program that
// drift apart. The bytes are still committed, because `go generate` is not part
// of anybody's build and a missing icon must not be able to break one.
//
// Run it with:
//
//	go generate ./internal/tray/...
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// size is the emitted icon size. 32×32 is the size Windows asks for at 100% DPI
// for the notification area and downscales cleanly to 16; going larger would
// weigh more in a binary that embeds five of these for a feature most builds do
// not compile at all.
const size = 32

// ss is the supersampling factor. Everything is drawn at size*ss and box-filtered
// down, which is the cheapest way to get antialiased edges with no dependency
// beyond image/draw.
const ss = 8

var (
	white = color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
	green = color.NRGBA{0x2E, 0x9E, 0x5B, 0xFF} // idle: up to date
	blue  = color.NRGBA{0x2F, 0x6F, 0xDB, 0xFF} // syncing: work in flight
	amber = color.NRGBA{0xD9, 0x94, 0x25, 0xFF} // paused: deliberately stopped
	red   = color.NRGBA{0xCC, 0x3B, 0x33, 0xFF} // error: needs attention
	grey  = color.NRGBA{0x8A, 0x8F, 0x98, 0xFF} // offline: no daemon to ask
)

func main() {
	dir := flag.String("out", ".", "directory to write the icons into")
	flag.Parse()

	icons := map[string]*image.NRGBA{
		"idle":    idle(),
		"syncing": syncing(),
		"paused":  paused(),
		"error":   erred(),
		"offline": offline(),
	}
	for name, img := range icons {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			fail(err)
		}
		write(filepath.Join(*dir, name+".png"), buf.Bytes())
		write(filepath.Join(*dir, name+".ico"), ico(img))
	}
}

func write(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", filepath.Base(path), len(data))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// --- the five states -------------------------------------------------------
//
// Each is a filled disc in the state's colour with a white mark on it. Colour
// alone would fail anyone who cannot separate red from green, and a mark alone
// would be unreadable at this size — so every state differs in both.

func idle() *image.NRGBA { // a tick
	c := newCanvas()
	disc(c, green)
	stroke(c, 0.30, 0.52, 0.44, 0.66, 0.10, white)
	stroke(c, 0.44, 0.66, 0.72, 0.36, 0.10, white)
	return finish(c)
}

func syncing() *image.NRGBA { // an arrow chasing its own circle
	c := newCanvas()
	disc(c, blue)
	ring(c, 0.30, math.Pi*0.35, math.Pi*1.9, 0.085, white)
	triangle(c, 0.50, 0.20, 0.72, 0.30, 0.46, 0.38, white)
	return finish(c)
}

func paused() *image.NRGBA { // two bars
	c := newCanvas()
	disc(c, amber)
	bar(c, 0.40, 0.32, 0.11, 0.36, white)
	bar(c, 0.60, 0.32, 0.11, 0.36, white)
	return finish(c)
}

func erred() *image.NRGBA { // a cross
	c := newCanvas()
	disc(c, red)
	stroke(c, 0.34, 0.34, 0.66, 0.66, 0.10, white)
	stroke(c, 0.66, 0.34, 0.34, 0.66, 0.10, white)
	return finish(c)
}

func offline() *image.NRGBA { // a dash: nothing to report, nobody to ask
	c := newCanvas()
	disc(c, grey)
	bar(c, 0.50, 0.50, 0.40, 0.11, white)
	return finish(c)
}

// --- primitives ------------------------------------------------------------
//
// Coordinates are fractions of the icon box, so the drawing code says nothing
// about pixels and the size constant above is the only thing to change.

func newCanvas() *image.NRGBA {
	return image.NewNRGBA(image.Rect(0, 0, size*ss, size*ss))
}

// px converts a fraction of the box to supersampled pixels.
func px(f float64) float64 { return f * float64(size*ss) }

// disc fills the icon's body, inset slightly so the antialiased edge is not
// clipped by the icon bounds.
func disc(c *image.NRGBA, col color.NRGBA) {
	cx, cy, r := px(0.5), px(0.5), px(0.47)
	forEach(c, func(x, y float64) float64 {
		return coverage(r - math.Hypot(x-cx, y-cy))
	}, col)
}

// stroke draws a round-capped line between two fractional points.
func stroke(c *image.NRGBA, x0, y0, x1, y1, w float64, col color.NRGBA) {
	ax, ay, bx, by, hw := px(x0), px(y0), px(x1), px(y1), px(w)/2
	forEach(c, func(x, y float64) float64 {
		return coverage(hw - distToSegment(x, y, ax, ay, bx, by))
	}, col)
}

// ring draws an arc of an annulus centred on the icon, from a0 to a1 radians.
func ring(c *image.NRGBA, radius, a0, a1, w float64, col color.NRGBA) {
	cx, cy, r, hw := px(0.5), px(0.5), px(radius), px(w)/2
	forEach(c, func(x, y float64) float64 {
		d := math.Hypot(x-cx, y-cy)
		ang := math.Atan2(y-cy, x-cx)
		if ang < 0 {
			ang += 2 * math.Pi
		}
		if ang < a0 || ang > a1 {
			return 0
		}
		return coverage(hw - math.Abs(d-r))
	}, col)
}

// bar draws an axis-aligned rectangle centred on (cxf, cyf).
func bar(c *image.NRGBA, cxf, cyf, wf, hf float64, col color.NRGBA) {
	cx, cy, hw, hh := px(cxf), px(cyf), px(wf)/2, px(hf)/2
	forEach(c, func(x, y float64) float64 {
		if math.Abs(x-cx) <= hw && math.Abs(y-cy) <= hh {
			return 1
		}
		return 0
	}, col)
}

// triangle fills the triangle through three fractional points.
func triangle(c *image.NRGBA, x0, y0, x1, y1, x2, y2 float64, col color.NRGBA) {
	ax, ay, bx, by, cx2, cy2 := px(x0), px(y0), px(x1), px(y1), px(x2), px(y2)
	forEach(c, func(x, y float64) float64 {
		d1 := sign(x, y, ax, ay, bx, by)
		d2 := sign(x, y, bx, by, cx2, cy2)
		d3 := sign(x, y, cx2, cy2, ax, ay)
		hasNeg := d1 < 0 || d2 < 0 || d3 < 0
		hasPos := d1 > 0 || d2 > 0 || d3 > 0
		if hasNeg && hasPos {
			return 0
		}
		return 1
	}, col)
}

func sign(px1, py1, ax, ay, bx, by float64) float64 {
	return (px1-bx)*(ay-by) - (ax-bx)*(py1-by)
}

// forEach composites col over every pixel, weighted by the alpha the shape
// function reports there.
func forEach(c *image.NRGBA, shape func(x, y float64) float64, col color.NRGBA) {
	b := c.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			a := shape(float64(x)+0.5, float64(y)+0.5)
			if a <= 0 {
				continue
			}
			c.SetNRGBA(x, y, over(color.NRGBA{col.R, col.G, col.B, uint8(a * 255)}, c.NRGBAAt(x, y)))
		}
	}
}

// coverage turns a signed distance in supersampled pixels into an alpha. The
// supersampler does the real antialiasing; this only keeps the hard edge from
// aliasing against itself before the downscale.
func coverage(d float64) float64 {
	switch {
	case d >= 0.5:
		return 1
	case d <= -0.5:
		return 0
	default:
		return d + 0.5
	}
}

func distToSegment(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(x-ax, y-ay)
	}
	t := ((x-ax)*dx + (y-ay)*dy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-(ax+t*dx), y-(ay+t*dy))
}

// over composites src onto dst, both non-premultiplied.
func over(src, dst color.NRGBA) color.NRGBA {
	sa := float64(src.A) / 255
	da := float64(dst.A) / 255
	oa := sa + da*(1-sa)
	if oa == 0 {
		return color.NRGBA{}
	}
	ch := func(s, d uint8) uint8 {
		v := (float64(s)*sa + float64(d)*da*(1-sa)) / oa
		return uint8(math.Round(math.Max(0, math.Min(255, v))))
	}
	return color.NRGBA{ch(src.R, dst.R), ch(src.G, dst.G), ch(src.B, dst.B), uint8(math.Round(oa * 255))}
}

// finish box-filters the supersampled canvas down to the emitted size.
func finish(c *image.NRGBA) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := float64(ss * ss)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					p := c.NRGBAAt(x*ss+sx, y*ss+sy)
					// Weight colour by alpha, or transparent pixels drag the edges toward
					// black as they average in.
					pa := float64(p.A) / 255
					r += float64(p.R) * pa
					g += float64(p.G) * pa
					b += float64(p.B) * pa
					a += pa
				}
			}
			if a == 0 {
				continue
			}
			out.SetNRGBA(x, y, color.NRGBA{
				uint8(math.Round(r / a)), uint8(math.Round(g / a)), uint8(math.Round(b / a)),
				uint8(math.Round(a / n * 255)),
			})
		}
	}
	return out
}

// --- .ico ------------------------------------------------------------------

// ico wraps one image as a single-entry Windows icon. The entry is an
// uncompressed 32-bit BMP rather than a PNG: PNG-compressed entries need Vista
// or later *and* a loader that asked for them, and the few kilobytes saved are
// not worth an icon that silently fails to appear on somebody's machine.
func ico(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()

	// The AND mask is 1bpp, each row padded to a 4-byte boundary. It is vestigial
	// for a 32-bit icon — the alpha channel is what actually gets used — but the
	// format requires it and some loaders still read it.
	maskRow := ((w + 31) / 32) * 4
	var dib bytes.Buffer
	// BITMAPINFOHEADER. The height is doubled: the format counts the colour
	// bitmap and the mask as one image.
	binary.Write(&dib, binary.LittleEndian, uint32(40))
	binary.Write(&dib, binary.LittleEndian, int32(w))
	binary.Write(&dib, binary.LittleEndian, int32(h*2))
	binary.Write(&dib, binary.LittleEndian, uint16(1))  // planes
	binary.Write(&dib, binary.LittleEndian, uint16(32)) // bits per pixel
	binary.Write(&dib, binary.LittleEndian, uint32(0))  // BI_RGB, no compression
	binary.Write(&dib, binary.LittleEndian, uint32(w*h*4+maskRow*h))
	binary.Write(&dib, binary.LittleEndian, int32(0)) // pixels per metre, unused
	binary.Write(&dib, binary.LittleEndian, int32(0))
	binary.Write(&dib, binary.LittleEndian, uint32(0)) // palette entries
	binary.Write(&dib, binary.LittleEndian, uint32(0))

	// BGRA, premultiplied by nothing, bottom-up.
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			p := img.NRGBAAt(x, y)
			dib.Write([]byte{p.B, p.G, p.R, p.A})
		}
	}
	// A zero mask means "opaque everywhere"; the alpha channel above is the real
	// transparency.
	dib.Write(make([]byte, maskRow*h))

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&out, binary.LittleEndian, uint16(1)) // type 1 = icon
	binary.Write(&out, binary.LittleEndian, uint16(1)) // one image
	out.WriteByte(byte(w % 256))                       // 0 would mean 256
	out.WriteByte(byte(h % 256))
	out.WriteByte(0) // palette colours
	out.WriteByte(0) // reserved
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(32))
	binary.Write(&out, binary.LittleEndian, uint32(dib.Len()))
	binary.Write(&out, binary.LittleEndian, uint32(22)) // offset: 6 + 16
	out.Write(dib.Bytes())
	return out.Bytes()
}
