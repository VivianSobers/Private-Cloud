package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Video thumbnails — the one thing in this package that needs a decoder.
//
// Metadata does not: mp4.go and mkv.go read plain header fields ahead of any
// codec. A THUMBNAIL is different in kind. There is no frame sitting in a
// header to copy out; producing one means decoding an inter-frame codec, which
// means a video decoder, which means either cgo against libav* or a
// subprocess — and this repository has already decided that question once, for
// OCR, and got a good answer:
//
//	extract shells out to `tesseract`. No cgo, so the worker stays a static
//	binary; a crash in the C library takes the subprocess and not the worker;
//	and where the binary is absent the feature reports that cleanly instead of
//	failing, so a deployment without it still runs.
//
// This is the same arrangement with ffmpeg, with one deliberate difference:
// tesseract is picked up from PATH, and ffmpeg is NOT. It is turned on only by
// PC_FFMPEG_PATH naming it. The reason is consent. Tesseract on a box is a
// strong signal somebody installed it to do OCR; ffmpeg is on half the world's
// machines as a transitive dependency of something else entirely, and video
// decoding is the largest hostile-input surface this system has. Enabling the
// riskiest component because a library happened to pull its dependency in is
// not an operator's decision, it is an accident that looks like one.
//
// Everything downstream of the frame is the EXISTING image path: the frame is
// handed to Render, so a video's thumb and preview are produced by the same
// scaler, at the same two edge bounds, with the same white matte and the same
// JPEG quality as a photo's, and land in media_variant as the same rows. That
// is what makes ?variant=thumb|preview work for a video with no change to the
// content route at all.

// ErrNoThumbnailer means video thumbnailing was asked for and is not
// configured. Like extract.ErrNoOCR it is a "skip", never a failure to retry: a
// deployment can legitimately choose not to decode video, and burning a job's
// whole retry budget discovering that again is worse than useless.
var ErrNoThumbnailer = errors.New("video thumbnails need ffmpeg, which is not configured")

// videoTimeout bounds one ffmpeg invocation.
//
// Extracting a single frame from the head of a file is a sub-second operation
// on anything real, so this is not a performance budget — it is the guarantee
// that a crafted file cannot pin the worker. A machine with one spare core has
// no way to recover from an unbounded decode except by being restarted.
const videoTimeout = 60 * time.Second

// videoWaitDelay bounds the wait AFTER the process has been killed.
//
// exec.CommandContext kills on cancellation, but Wait still blocks until the
// stderr pipe closes, and a process that leaked a child holding that pipe would
// hang the goroutine forever with the job lease renewing behind it. WaitDelay
// turns that into an error. The requirement is "no hangs", and a timeout that
// only covers the easy half does not meet it.
const videoWaitDelay = 10 * time.Second

// frameSeeks are the offsets tried, in order, until one yields a decodable
// frame.
//
// One second first because the frame at t=0 is very often useless: phones fade
// in from black, cameras start on a lens cap, and screen recordings start on an
// empty desktop. Zero second, because a clip shorter than a second has no
// one-second mark and would otherwise produce no tile at all — which is the
// case a "just seek a bit in" heuristic silently gets wrong.
var frameSeeks = []string{"00:00:01", "00:00:00"}

// Thumbnailer renders a still from a video. Safe for concurrent use: every call
// gets its own temporary directory, and the binary path is resolved once at
// construction — the same shape extract.Extractor has.
type Thumbnailer struct {
	ffmpeg  string // resolved binary path, or "" when thumbnailing is off
	timeout time.Duration
}

// NewThumbnailer resolves the configured ffmpeg binary. An empty path — the
// default — yields a Thumbnailer that is simply unavailable, which every caller
// already has to handle because a rendering failure is survivable anyway.
//
// The value is passed through exec.LookPath rather than used verbatim, so it
// accepts both an absolute path (verified to be executable, catching the
// typo'd mount at startup rather than on the first video) and a bare name
// resolved on PATH.
func NewThumbnailer(path string) *Thumbnailer {
	t := &Thumbnailer{timeout: videoTimeout}
	if path == "" {
		return t
	}
	if resolved, err := exec.LookPath(path); err == nil {
		t.ffmpeg = resolved
	}
	return t
}

// Available reports whether video thumbnails can be rendered here. Worth
// logging at startup: "configured but the binary is missing" and "not
// configured" are the same behaviour and very different mistakes.
func (t *Thumbnailer) Available() bool { return t != nil && t.ffmpeg != "" }

// ExpectedVariants names the renditions a video of these dimensions should
// have, mirroring ExpectedVariants for images.
//
// Deliberately a method rather than a change to the package-level function.
// That one is also what fsck asks "are this file's variants complete", and a
// deployment with no ffmpeg would otherwise be told every video in the library
// is missing thumbnails — a permanently red check for work it has decided not
// to do. What is expected of a video depends on whether the machine can render
// one, and only something holding the Thumbnailer knows that.
//
// Unknown dimensions yield nothing: a video whose header neither parser could
// read has no known size, so there is no honest answer to "should a 320px copy
// of it exist", and guessing yes would re-attempt the same undecodable file on
// every pass forever.
func (t *Thumbnailer) ExpectedVariants(contentType string, width, height int) []string {
	if !t.Available() || !isVideo(contentType) || width <= 0 || height <= 0 {
		return nil
	}
	return variantsForEdge(longestEdge(width, height))
}

// Render extracts one frame and turns it into the named variants.
//
// The frame goes through Render — the image path — rather than through any
// video-specific resizing, so what lands in media_variant for a video is the
// same kind of row, at the same sizes, as what lands there for a photo.
//
// data is the bounded prefix the job already read. That is deliberate and it
// has a cost worth naming: the frames near t=0 live at the start of mdat, so
// the prefix is exactly the part of the file that contains them — but a
// non-faststart recording keeps its moov at the end, and ffmpeg cannot demux
// what it cannot find a header for. Such a file gets its metadata (parseMP4Seek
// reaches the header with a seek) and no thumbnail. Copying gigabytes to a
// temporary file to close that gap would trade a missing tile for hours of disk
// IO on a box with one spinning disk, which is the worse deal.
func (t *Thumbnailer) Render(ctx context.Context, contentType string, data []byte, names ...string) ([]Variant, error) {
	if !t.Available() {
		return nil, ErrNoThumbnailer
	}
	if !isVideo(contentType) {
		return nil, ErrUnsupported
	}

	frame, err := t.frame(ctx, data)
	if err != nil {
		return nil, err
	}

	// PNG, so the frame reaching the scaler is exactly what was decoded. Going
	// through an intermediate JPEG would encode the picture twice for no gain:
	// the artifacts of the first pass are what the second one then sharpens.
	//
	// Render re-reads the header, so the MaxPixels bomb guard applies to a
	// frame ffmpeg produced on the same terms it applies to an uploaded PNG —
	// which matters, because the dimensions came from a header the file chose.
	variants, err := Render("image/png", frame, names...)
	if err != nil {
		return nil, err
	}
	return variants, nil
}

// frame runs ffmpeg until one seek offset yields a still.
func (t *Thumbnailer) frame(ctx context.Context, data []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "pc-video-*")
	if err != nil {
		return nil, err
	}
	// One directory per call, removed whole: no shared filenames, so two
	// concurrent jobs cannot collide, and nothing survives a failure.
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, "in")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "frame.png")

	var lastErr error
	for _, seek := range frameSeeks {
		// Removed between attempts so a zero-byte file left by a failed seek is
		// never mistaken for the frame the next one was supposed to produce.
		_ = os.Remove(out)

		if err := t.run(ctx, seek, in, out); err != nil {
			lastErr = err
			continue
		}
		frame, err := os.ReadFile(out)
		if err != nil || len(frame) == 0 {
			// ffmpeg exits 0 having written nothing when the seek lands past the
			// end of a short clip. That is not a failure, it is the next offset's
			// turn.
			lastErr = fmt.Errorf("%w: ffmpeg produced no frame at %s", ErrDecode, seek)
			continue
		}
		return frame, nil
	}
	return nil, lastErr
}

// run invokes ffmpeg for a single frame at one offset.
func (t *Thumbnailer) run(ctx context.Context, seek, in, out string) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// -ss BEFORE -i is the fast seek: ffmpeg jumps to the nearest keyframe
	// rather than decoding everything up to the offset, which is the difference
	// between reading a few hundred kilobytes and decoding a whole minute of
	// video to throw it away.
	//
	// -an -sn drop audio and subtitles, which nothing here wants and which a
	// crafted file could otherwise make us decode. -nostdin because an ffmpeg
	// that decides to prompt would otherwise wait on a stdin nobody is going to
	// write to — the timeout would catch it, a minute later, every time.
	cmd := exec.CommandContext(ctx, t.ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-ss", seek,
		"-i", in,
		"-frames:v", "1", "-an", "-sn",
		"-f", "image2", "-c:v", "png",
		"-y", out,
	)
	cmd.WaitDelay = videoWaitDelay

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("video thumbnail timed out: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, truncateForLog(errBuf.String()))
	}
	return nil
}

// truncateForLog bounds what a subprocess's stderr can contribute to an error
// string. ffmpeg is verbose about a file it dislikes, and that text ends up in
// a log line and in the jobs table's last_error column.
func truncateForLog(s string) string {
	const max = 400
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
