package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests below drive a FAKE ffmpeg: this test binary, re-executed with
// PC_FAKE_FFMPEG set, which recognises the argument list video.go builds and
// writes a PNG where it was told to.
//
// Re-executing ourselves rather than compiling a shell script keeps the test
// cross-platform — this repository is developed on Windows and runs on Debian —
// and it makes the fake's behaviour part of the test file that depends on it.
// What is under test is everything around the subprocess: that the enabled path
// produces the SAME variant kinds and shapes the image path produces, that the
// disabled path is silent rather than broken, and that a binary which fails,
// writes nothing, or refuses to exit degrades instead of taking the worker with
// it.

func TestMain(m *testing.M) {
	// Checked before flag parsing, because the arguments here are ffmpeg's and
	// the testing package would reject them.
	if mode := os.Getenv("PC_FAKE_FFMPEG"); mode != "" {
		os.Exit(fakeFFmpeg(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeFFmpeg implements just enough of the real thing for the arguments
// video.go passes, and each mode is one real failure being reproduced.
func fakeFFmpeg(mode string, args []string) int {
	var out, seek string
	for i, a := range args {
		switch a {
		case "-y":
			if i+1 < len(args) {
				out = args[i+1]
			}
		case "-ss":
			if i+1 < len(args) {
				seek = args[i+1]
			}
		}
	}

	switch mode {
	case "fail":
		os.Stderr.WriteString(strings.Repeat("moov atom not found; ", 200))
		return 1
	case "hang":
		// The file that never finishes. The timeout is the only thing that ends
		// this, which is the property being tested.
		time.Sleep(10 * time.Minute)
		return 0
	case "empty":
		// ffmpeg exits 0 having written nothing. Real behaviour when a seek
		// lands past the end of a clip.
		return 0
	case "short":
		// A clip under a second: no frame at 00:00:01, a frame at 00:00:00.
		if seek != "00:00:00" {
			return 0
		}
	}

	if out == "" {
		return 1
	}
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	// Not a uniform fill: a flat image encodes to almost nothing, and "the
	// bytes arrived" should not be provable by an empty file.
	for y := 0; y < 1080; y += 3 {
		for x := 0; x < 1920; x += 3 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 0x40, 0xFF})
		}
	}
	f, err := os.Create(out)
	if err != nil {
		return 1
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return 1
	}
	return 0
}

// fakeThumbnailer points a Thumbnailer at this test binary in the given mode.
func fakeThumbnailer(t *testing.T, mode string) *Thumbnailer {
	t.Helper()
	// Inherited by the subprocess, which is what makes it act as ffmpeg. Set on
	// the parent too, harmlessly: TestMain has already run.
	t.Setenv("PC_FAKE_FFMPEG", mode)

	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary to use as a fake ffmpeg: %v", err)
	}
	return NewThumbnailer(self)
}

// A video's variants must be indistinguishable in kind and shape from a photo's,
// because ?variant=thumb|preview, the content route, the ETag and the share
// plane all treat them as one thing. Anything else means a second byte-serving
// path to review.
func TestVideoThumbnailsMatchTheImageVariants(t *testing.T) {
	th := fakeThumbnailer(t, "ok")
	if !th.Available() {
		t.Fatal("a thumbnailer pointed at an existing executable reports unavailable")
	}

	got, err := th.Render(context.Background(), "video/mp4", []byte("pretend this is an mp4"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The same 1920x1080 source through the image path, which is the definition
	// of "the same shapes" rather than a set of numbers copied into a test.
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	want, err := Render("image/png", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(want) {
		t.Fatalf("rendered %d variants, want the %d an image of the same size produces", len(got), len(want))
	}
	for i := range got {
		if got[i].Name != want[i].Name || got[i].MIME != want[i].MIME ||
			got[i].Width != want[i].Width || got[i].Height != want[i].Height {
			t.Errorf("variant %d = %+v, want the shape of %+v", i, got[i], want[i])
		}
		if len(got[i].Data) == 0 {
			t.Errorf("variant %q has no bytes", got[i].Name)
		}
	}
}

// The default. Nothing is configured, nothing runs, and the caller is told
// which of the two it is — a sentinel the handler reads as "skip", never as a
// failure to retry.
func TestVideoThumbnailsDisabledByDefault(t *testing.T) {
	th := NewThumbnailer("")
	if th.Available() {
		t.Fatal("video thumbnails are on with no PC_FFMPEG_PATH set")
	}
	if _, err := th.Render(context.Background(), "video/mp4", []byte("anything")); !errors.Is(err, ErrNoThumbnailer) {
		t.Errorf("Render = %v, want ErrNoThumbnailer", err)
	}
	if v := th.ExpectedVariants("video/mp4", 1920, 1080); v != nil {
		t.Errorf("expected variants = %v, want none where nothing can render them", v)
	}
	// A nil Thumbnailer is the handler's representation of "off", so it must
	// behave like a disabled one rather than panicking.
	var nilth *Thumbnailer
	if nilth.Available() {
		t.Error("a nil thumbnailer reports available")
	}
	if _, err := nilth.Render(context.Background(), "video/mp4", nil); !errors.Is(err, ErrNoThumbnailer) {
		t.Errorf("nil Render = %v, want ErrNoThumbnailer", err)
	}
	if v := nilth.ExpectedVariants("video/mp4", 1920, 1080); v != nil {
		t.Errorf("nil expected variants = %v, want none", v)
	}
}

// A path naming nothing is the same behaviour as no path — the operator finds
// out from the startup log, not from a worker that will not start.
func TestThumbnailerWithAMissingBinaryIsUnavailable(t *testing.T) {
	th := NewThumbnailer(filepath.Join(t.TempDir(), "no-such-ffmpeg"))
	if th.Available() {
		t.Error("a thumbnailer pointed at a missing file reports available")
	}
}

// What a video should have depends on what the machine can render, and on
// dimensions it actually read — never on a guess.
func TestVideoExpectedVariantsFollowTheImageRule(t *testing.T) {
	th := fakeThumbnailer(t, "ok")
	for _, tc := range []struct {
		name          string
		mime          string
		width, height int
		want          []string
	}{
		{"a 1080p recording gets both", "video/mp4", 1920, 1080, []string{VariantPreview, VariantThumb}},
		{"a small clip gets only a thumb", "video/webm", 640, 480, []string{VariantThumb}},
		{"a tiny clip needs no copy at all", "video/mp4", 240, 180, nil},
		{"unknown dimensions promise nothing", "video/x-matroska", 0, 0, nil},
		{"an image is not this method's business", "image/jpeg", 4000, 3000, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := th.ExpectedVariants(tc.mime, tc.width, tc.height)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("expected variants = %v, want %v", got, tc.want)
			}
		})
	}
	// The rule is shared, so a video and an image of one size agree by
	// construction rather than by two lists being kept in step.
	if a, b := th.ExpectedVariants("video/mp4", 1920, 1080), ExpectedVariants("image/jpeg", 1920, 1080); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("video %v and image %v disagree about the same dimensions", a, b)
	}
}

// The frame at t=0 is often a black fade-in, so one second is tried first — but
// a clip shorter than that must still produce a tile rather than nothing.
func TestShortClipFallsBackToTheFirstFrame(t *testing.T) {
	th := fakeThumbnailer(t, "short")
	got, err := th.Render(context.Background(), "video/mp4", []byte("a very short clip"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(got) == 0 {
		t.Error("a clip shorter than the first seek offset produced no variants")
	}
}

// Everything a hostile or simply unreadable file can do to the subprocess, and
// none of it may become a retried job, a panic, or a worker that never returns.
func TestVideoThumbnailFailuresDegrade(t *testing.T) {
	for _, mode := range []string{"fail", "empty"} {
		t.Run(mode, func(t *testing.T) {
			th := fakeThumbnailer(t, mode)
			got, err := th.Render(context.Background(), "video/mp4", []byte("unreadable"))
			if err == nil {
				t.Fatalf("expected an error, got %d variants", len(got))
			}
			if errors.Is(err, ErrNoThumbnailer) {
				t.Error("a failing binary was reported as an unconfigured one")
			}
			// The error reaches jobs.last_error and the logs, so ffmpeg's
			// enthusiasm about a file it dislikes must not arrive whole.
			if len(err.Error()) > 600 {
				t.Errorf("error is %d bytes; subprocess stderr is not bounded", len(err.Error()))
			}
		})
	}
}

// The bound that matters most: a file that makes the decoder never finish must
// cost one job, not the worker. The timeout is shortened here because the
// property under test is that there IS one.
func TestVideoThumbnailTimesOut(t *testing.T) {
	th := fakeThumbnailer(t, "hang")
	th.timeout = 300 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := th.Render(context.Background(), "video/mp4", []byte("never finishes"))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hanging decoder returned success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a hanging decoder was not killed; the timeout does not work")
	}
}

// A cancelled job — the worker shutting down, the lease lost — stops the
// subprocess rather than outliving the thing that started it.
func TestVideoThumbnailHonoursCancellation(t *testing.T) {
	th := fakeThumbnailer(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := th.Render(ctx, "video/mp4", []byte("never finishes"))
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled render returned success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancellation did not stop the subprocess")
	}
}

// The thumbnailer renders video and nothing else. An image reaching it would
// mean the handler routed by something other than content type.
func TestThumbnailerDeclinesNonVideo(t *testing.T) {
	th := fakeThumbnailer(t, "ok")
	if _, err := th.Render(context.Background(), "image/jpeg", []byte("not a video")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Render(image/jpeg) = %v, want ErrUnsupported", err)
	}
}
