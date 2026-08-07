package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRenderProducesBothVariants(t *testing.T) {
	src := encodeJPEG(t, 4000, 3000)

	vs, err := Render("image/jpeg", src)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("got %d variants, want 2", len(vs))
	}

	byName := map[string]Variant{}
	for _, v := range vs {
		byName[v.Name] = v
	}
	thumb, preview := byName[VariantThumb], byName[VariantPreview]

	// Longest edge is the bound, and aspect ratio is preserved: 4000x3000 is 4:3,
	// so a 320-wide thumb is 240 tall.
	if thumb.Width != ThumbMaxEdge || thumb.Height != 240 {
		t.Errorf("thumb = %dx%d, want 320x240", thumb.Width, thumb.Height)
	}
	if preview.Width != PreviewMaxEdge || preview.Height != 1200 {
		t.Errorf("preview = %dx%d, want 1600x1200", preview.Width, preview.Height)
	}

	// A thumbnail that is not dramatically smaller than the original is not doing
	// its job — the point is that a grid of them is cheap to load.
	if len(thumb.Data) >= len(src)/4 {
		t.Errorf("thumb is %d bytes against a %d byte original", len(thumb.Data), len(src))
	}
	if len(thumb.Data) >= len(preview.Data) {
		t.Errorf("thumb (%d) should be smaller than preview (%d)", len(thumb.Data), len(preview.Data))
	}

	// And the bytes must actually be a decodable JPEG of the claimed size.
	for _, v := range vs {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(v.Data))
		if err != nil {
			t.Fatalf("%s is not decodable: %v", v.Name, err)
		}
		if cfg.Width != v.Width || cfg.Height != v.Height {
			t.Errorf("%s claims %dx%d, decodes as %dx%d", v.Name, v.Width, v.Height, cfg.Width, cfg.Height)
		}
		if v.MIME != "image/jpeg" {
			t.Errorf("%s mime = %q", v.Name, v.MIME)
		}
	}
}

// An image already smaller than a target needs no copy of that size: it would be
// the same pixels in a new file, costing disk and a GC pass to reclaim.
func TestRenderSkipsVariantsThatWouldNotShrink(t *testing.T) {
	// Between the thumb and preview bounds: needs a thumb, not a preview.
	vs, err := Render("image/jpeg", encodeJPEG(t, 800, 600))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(vs) != 1 || vs[0].Name != VariantThumb {
		t.Fatalf("got %v, want a thumb only", names(vs))
	}

	// Smaller than both: no variants at all, and that is success, not an error.
	vs, err = Render("image/jpeg", encodeJPEG(t, 100, 80))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("a tiny image needs no variants, got %v", names(vs))
	}
}

func TestRenderSelectsNamedVariants(t *testing.T) {
	vs, err := Render("image/jpeg", encodeJPEG(t, 4000, 3000), VariantThumb)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(vs) != 1 || vs[0].Name != VariantThumb {
		t.Errorf("got %v, want thumb only", names(vs))
	}
}

// A transparent PNG must not encode its transparent regions as black, which is
// what happens when RGBA with alpha goes straight into a JPEG.
func TestRenderFlattensTransparencyOntoWhite(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 900, 900))
	// Fully transparent everywhere.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	vs, err := Render("image/png", buf.Bytes(), VariantThumb)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("got %d variants", len(vs))
	}
	out, err := jpeg.Decode(bytes.NewReader(vs[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := out.At(10, 10).RGBA()
	// Near-white, allowing for JPEG's lossiness.
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("transparent pixel became rgb(%d,%d,%d), want near-white", r>>8, g>>8, b>>8)
	}
}

// An extreme aspect ratio must not round a dimension to zero, which no encoder
// accepts.
func TestFitNeverReturnsZero(t *testing.T) {
	cases := [][4]int{
		{4000, 1, 320, 320}, // w, h, maxEdge, expected width
		{1, 4000, 320, 1},   // expected width for the tall case
		{4000, 3000, 320, 320},
	}
	for _, c := range cases {
		w, h := fit(c[0], c[1], c[2])
		if w < 1 || h < 1 {
			t.Errorf("fit(%d,%d,%d) = %dx%d, must never be zero", c[0], c[1], c[2], w, h)
		}
		if w > c[2] || h > c[2] {
			t.Errorf("fit(%d,%d,%d) = %dx%d exceeds the bound", c[0], c[1], c[2], w, h)
		}
	}
	// And the whole path works end to end on a degenerate strip.
	vs, err := Render("image/jpeg", encodeJPEG(t, 2000, 1), VariantThumb)
	if err != nil {
		t.Fatalf("Render on a 2000x1 strip: %v", err)
	}
	if len(vs) != 1 || vs[0].Height < 1 {
		t.Errorf("degenerate strip produced %v", vs)
	}
}

func TestRenderRejectsNonImages(t *testing.T) {
	if _, err := Render("application/pdf", []byte("%PDF")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if _, err := Render("image/jpeg", []byte("not a jpeg")); !errors.Is(err, ErrDecode) {
		t.Errorf("err = %v, want ErrDecode", err)
	}
}

func names(vs []Variant) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Name
	}
	return out
}
