// Package media reads what a photo or video is — its dimensions, when it was
// taken, which way up it goes, where it was — and renders the smaller copies a
// gallery needs.
//
// Like the extract package it is pure of the database and the queue: it takes
// bytes and a content type and returns values. That is what lets the same code
// run in the co-located worker and in a test with a synthetic JPEG, and it keeps
// the decoding — the part that runs on hostile input — in one reviewable place.
//
// Nothing here shells out and nothing here is a model. Image decoding is the Go
// standard library; EXIF is a small reader in this package. A video's duration
// needs a container parser, which is why video metadata is currently limited to
// what a probe can determine without one.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"

	// Registered for their side effects: image.DecodeConfig and image.Decode
	// dispatch on the formats that have been imported.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

var (
	// ErrUnsupported means this content type is not media this package handles.
	// Not a failure to retry — a PDF simply has no thumbnail.
	ErrUnsupported = errors.New("not a supported media type")
	// ErrDecode means the bytes claimed to be an image and could not be decoded.
	// Also not worth retrying: the file will not improve.
	ErrDecode = errors.New("could not decode this image")
)

// MaxInputBytes caps what will be read into memory to analyse.
//
// Lower than the extractor's 64 MiB because decoding is not a streaming
// operation: a decoded image costs width x height x 4 bytes regardless of how
// well the file compressed, so the file size is a poor proxy for the memory the
// decode will actually want. The pixel bound below is the real guard; this one
// stops the read itself.
const MaxInputBytes = 40 << 20

// MaxPixels bounds the decoded image.
//
// This is the decompression-bomb guard. A 100-megapixel PNG is a few hundred
// kilobytes of highly compressible black and 400 MB of decoded RGBA — on a 7 GiB
// box that is the whole worker. 80 megapixels is comfortably above any real
// camera (a 100MP phone panorama is ~50MP) and far below the point where one
// file can take the process down.
const MaxPixels = 80_000_000

// Metadata is what could be determined about one file. Every field is optional:
// a screenshot has no camera, a PNG has no capture time, an image has no
// duration. Absent is normal, not an error.
type Metadata struct {
	Width, Height int
	// Orientation is the EXIF flag, 1-8. Always at least 1 ("as stored") so a
	// client never has to special-case zero.
	Orientation int
	TakenAt     *time.Time
	Camera      string
	GPSLat      *float64
	GPSLon      *float64
	DurationMS  *int64
	// Source names what produced this — "image" or "video" — so a later, better
	// analyser can be told which rows are worth redoing.
	Source string
}

// IsMedia reports whether a content type is one this package can analyse.
func IsMedia(contentType string) bool {
	return isImage(contentType) || isVideo(contentType)
}

func mediaType(contentType string) string {
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

// isImage reports whether this is a raster format the standard library decodes.
// Deliberately an allowlist rather than a `image/` prefix test: SVG is
// `image/*` and is not a raster image, and a format we cannot decode should be
// declined up front rather than after a failed decode.
func isImage(contentType string) bool {
	switch mediaType(contentType) {
	case "image/jpeg", "image/jpg", "image/png", "image/gif":
		return true
	}
	return false
}

func isVideo(contentType string) bool {
	return strings.HasPrefix(mediaType(contentType), "video/")
}

// Analyze reads a file's media metadata.
//
// It does NOT fully decode the image: image.DecodeConfig reads only the header,
// so dimensions cost a few hundred bytes of work rather than a full raster. The
// pixel bound is checked against those dimensions BEFORE anything decodes, which
// is what makes the bomb guard actually preventive.
func Analyze(contentType string, data []byte) (Metadata, error) {
	switch {
	case isImage(contentType):
		return analyzeImage(data)
	case isVideo(contentType):
		return analyzeVideo(data)
	default:
		return Metadata{}, ErrUnsupported
	}
}

func analyzeImage(data []byte) (Metadata, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Metadata{}, ErrDecode
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Metadata{}, fmt.Errorf("%w: %dx%d exceeds the %d pixel limit",
			ErrDecode, cfg.Width, cfg.Height, MaxPixels)
	}

	// EXIF is best effort and never fails the analysis: a photo with a corrupt
	// metadata block still has usable dimensions, and dimensions are what the
	// gallery actually needs to lay out a grid.
	m := parseEXIF(data)
	m.Width, m.Height = cfg.Width, cfg.Height
	if m.Orientation == 0 {
		m.Orientation = 1
	}
	m.Source = "image"
	return m, nil
}

// analyzeVideo reads what the container header can tell us, and records the file
// as video regardless.
//
// MP4 and QuickTime keep duration, dimensions, rotation and capture time in the
// `moov` header as plain fields, ahead of any codec, so mp4.go reads them
// without a demuxer and without the cgo dependency a THUMBNAIL would need — that
// one still requires a video decoder, and Render still declines video.
//
// Two cases legitimately yield nothing but the bare record, and neither is an
// error:
//
//   - A container this parser does not read. Matroska and WebM keep the same
//     facts in an EBML tree, which is a different parser; until it exists those
//     files behave as every video did before.
//   - An MP4 whose `moov` sits after the media data. Writing the header last is
//     normal for a recording, "faststart" is what moves it to the front, and
//     this package is handed a bounded PREFIX of the file — so a long recording
//     can genuinely have its header beyond what was read.
//
// Returning a bare record rather than an error is deliberate in both: it marks
// the file as media so a timeline includes it, ordered by upload time until
// something can tell it when the video was actually shot. `source` is what a
// later, better analyser uses to find the rows worth redoing.
func analyzeVideo(data []byte) (Metadata, error) {
	if looksLikeMP4(data) {
		if m, ok := parseMP4(data); ok {
			return m, nil
		}
	}
	return Metadata{Orientation: 1, Source: "video"}, nil
}
