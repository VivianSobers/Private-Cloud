package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	xdraw "golang.org/x/image/draw"
)

// white is the matte transparent images are flattened onto before JPEG encoding.
var white = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}

// Derived renditions.
//
// Two sizes, not a free-form resize endpoint: a grid tile and a lightbox image.
// An arbitrary ?w= parameter would mean rendering on demand, which puts image
// decoding on the request path of the always-on box — exactly what the job queue
// exists to keep off it. Two fixed sizes are generated once, cached by content
// hash, and served as static bytes.

// Variant names. These are the values stored in media_variant.variant and
// accepted by the ?variant= query parameter.
const (
	VariantThumb   = "thumb"
	VariantPreview = "preview"
)

// Longest-edge targets.
//
// 320 is a grid tile at 2x on a phone; 1600 covers a lightbox on a 1080p or
// 1440p display without being a meaningful fraction of the original. Both are
// upper bounds on the LONGEST edge, so aspect ratio is always preserved and a
// panorama does not become a square.
const (
	ThumbMaxEdge   = 320
	PreviewMaxEdge = 1600
)

// jpegQuality for derived images. 82 is the usual knee: visually
// indistinguishable from 95 at these sizes and roughly half the bytes, which is
// what a grid of 200 tiles actually cares about.
const jpegQuality = 82

// ErrNoVariantNeeded means the source is already at or below the target size, so
// a "smaller" copy would be the same pixels in a new file — pure waste of disk
// and of the GC that would later have to reclaim it.
var ErrNoVariantNeeded = errors.New("image is already smaller than this variant")

// Variant is a rendered rendition ready to store.
type Variant struct {
	Name          string
	MIME          string
	Width, Height int
	Data          []byte
}

// VariantSpecs returns the variants to generate, largest first.
func VariantSpecs() []struct {
	Name    string
	MaxEdge int
} {
	return []struct {
		Name    string
		MaxEdge int
	}{
		{VariantPreview, PreviewMaxEdge},
		{VariantThumb, ThumbMaxEdge},
	}
}

// ExpectedVariants names the renditions that SHOULD exist for content of these
// dimensions — the same decision renderOne makes, taken from stored metadata
// instead of from a decoded image.
//
// It exists so "have the variants been rendered?" is answerable without opening
// the file. The naive test, "are there any variant rows", is wrong in a way that
// matters: an image smaller than a thumbnail legitimately has none, because a
// "smaller" copy of it would be the same pixels in a new file. Treating that as
// unfinished would re-render every icon in the library on every pass, forever.
//
// Video returns nil HERE, and that is now a statement about this function
// rather than about video: rendering a video tile needs a decoder, so what a
// video should have depends on whether ffmpeg is configured on the machine
// asking. Thumbnailer.ExpectedVariants answers that; this one is also what fsck
// consults, and fsck must not report every video in the library as incomplete
// on a deployment that deliberately does not decode video.
func ExpectedVariants(contentType string, width, height int) []string {
	if !isImage(contentType) || width <= 0 || height <= 0 {
		return nil
	}
	return variantsForEdge(longestEdge(width, height))
}

// variantsForEdge is the size rule itself, shared with the video path so a
// video's expectations cannot drift from an image's. A source at or below a
// target needs no copy of it — see ErrNoVariantNeeded.
func variantsForEdge(longest int) []string {
	var out []string
	for _, spec := range VariantSpecs() {
		if longest > spec.MaxEdge {
			out = append(out, spec.Name)
		}
	}
	return out
}

func longestEdge(width, height int) int {
	if height > width {
		return height
	}
	return width
}

// Render decodes an image once and produces the named variants from it.
//
// One decode for both sizes, deliberately: decoding is the expensive half, and
// doing it per variant would double the cost of the single most expensive thing
// this worker does. The preview is rendered from the full-size source rather
// than the thumbnail from the preview, so quality does not compound losses.
func Render(contentType string, data []byte, names ...string) ([]Variant, error) {
	if !isImage(contentType) {
		return nil, ErrUnsupported
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	// Guard before decoding, not after: see MaxPixels.
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, fmt.Errorf("%w: %dx%d exceeds the %d pixel limit",
			ErrDecode, cfg.Width, cfg.Height, MaxPixels)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}

	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}

	var out []Variant
	for _, spec := range VariantSpecs() {
		if len(names) > 0 && !want[spec.Name] {
			continue
		}
		v, err := renderOne(src, spec.Name, spec.MaxEdge)
		if errors.Is(err, ErrNoVariantNeeded) {
			continue // an original smaller than the target needs no copy
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func renderOne(src image.Image, name string, maxEdge int) (Variant, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return Variant{}, ErrDecode
	}
	if w <= maxEdge && h <= maxEdge {
		return Variant{}, ErrNoVariantNeeded
	}

	tw, th := fit(w, h, maxEdge)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))

	// Flatten onto white first. A PNG with transparency downscaled straight into
	// RGBA keeps its alpha, and a JPEG has no alpha channel to put it in — the
	// transparent regions would encode as black, turning a logo on a transparent
	// background into a logo on a black square.
	draw.Draw(dst, dst.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)

	// CatmullRoll is the quality/cost knee for downscaling: visibly better than
	// ApproxBiLinear on the 8-20x reductions a thumbnail actually involves, and
	// far cheaper than rendering these on demand would be at any quality.
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Variant{}, err
	}
	return Variant{
		Name:   name,
		MIME:   "image/jpeg",
		Width:  tw,
		Height: th,
		Data:   buf.Bytes(),
	}, nil
}

// fit scales w x h down so the longest edge is maxEdge, preserving aspect ratio
// and never returning a zero dimension — a 4000x1 panorama must still produce a
// 320x1 image rather than a 320x0 one that no encoder will accept.
func fit(w, h, maxEdge int) (int, int) {
	if w >= h {
		nh := h * maxEdge / w
		if nh < 1 {
			nh = 1
		}
		return maxEdge, nh
	}
	nw := w * maxEdge / h
	if nw < 1 {
		nw = 1
	}
	return nw, maxEdge
}
