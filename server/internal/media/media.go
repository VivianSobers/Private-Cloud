// Package media reads what a photo or video is — its dimensions, when it was
// taken, which way up it goes, where it was — and renders the smaller copies a
// gallery needs.
//
// Like the extract package it is pure of the database and the queue: it takes
// bytes and a content type and returns values. That is what lets the same code
// run in the co-located worker and in a test with a synthetic JPEG, and it keeps
// the decoding — the part that runs on hostile input — in one reviewable place.
//
// Nothing here is a model, and everything a deployment gets by default runs
// in-process: image decoding is the Go standard library, EXIF is a small reader
// in this package, and video metadata is two container parsers — mp4.go and
// mkv.go — reading plain header fields rather than demuxing.
//
// Exactly one thing shells out, and it is off unless an operator turns it on: a
// video THUMBNAIL needs a real decoder, so video.go drives ffmpeg as a
// subprocess when PC_FFMPEG_PATH names one, on the same terms extraction drives
// tesseract. No cgo, a crash takes the subprocess rather than the worker, and a
// deployment without it renders every photo thumbnail exactly as before.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
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
	return AnalyzeSource(contentType, data, nil)
}

// AnalyzeSource is Analyze with the whole file still reachable.
//
// src is optional and is only ever consulted for a video whose header was not
// in the prefix — see analyzeVideo. Images are unaffected: a decoder reads a
// header from the front of the file, so there is nothing past the prefix an
// image could want, and handing this a nil src is a perfectly ordinary call.
//
// It exists because "the caller has the bytes" and "the caller can reach the
// file" are genuinely different situations and the difference changes what can
// be reported. A test with a synthetic buffer stays a one-argument call; the
// worker, whose opener returns an io.ReadSeekCloser because the download path
// needs Range support anyway, passes the reader and gets the metadata a
// non-faststart recording keeps at its end.
func AnalyzeSource(contentType string, data []byte, src io.ReadSeeker) (Metadata, error) {
	switch {
	case isImage(contentType):
		return analyzeImage(data)
	case isVideo(contentType):
		return analyzeVideo(data, src)
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
// Both container families keep duration, dimensions, rotation and capture time
// as plain fields ahead of any codec — MP4 and QuickTime in the `moov` box tree,
// Matroska and WebM in the EBML tree — so mp4.go and mkv.go read them without a
// demuxer and without the dependency a THUMBNAIL needs. That one does need a
// video decoder, which is why it lives behind the ffmpeg switch in video.go and
// why Render, which only ever decodes in-process, still declines video.
//
// The prefix bound only bites one of the two. Matroska requires Info and Tracks
// before the first Cluster, so a prefix that reaches the Segment header reaches
// everything read here. MP4 has no such rule: a recording that was not written
// "faststart" carries `moov` after the media data, and on a file longer than
// MaxInputBytes the header is genuinely past the end of what was read. When the
// caller can still reach the file, parseMP4Seek walks the top-level boxes to
// find it — a handful of seeks, never a scan, and mdat is never read.
//
// What is left is a bare record: a container neither parser recognises, a
// truncated file, or a non-faststart MP4 supplied as bytes alone. That is not
// an error. It marks the file as media so a timeline includes it, ordered by
// upload time until something can say when the video was actually shot, and
// `source` is what a later, better analyser uses to find the rows worth redoing.
func analyzeVideo(data []byte, src io.ReadSeeker) (Metadata, error) {
	if looksLikeMatroska(data) {
		if m, ok := parseMKV(data); ok {
			return m, nil
		}
	}
	if looksLikeMP4(data) {
		if m, ok := parseMP4(data); ok {
			return m, nil
		}
		// The prefix was an MP4 and held no reachable moov. Ask the file.
		if src != nil {
			if m, ok := parseMP4Seek(src); ok {
				return m, nil
			}
		}
	}
	return Metadata{Orientation: 1, Source: "video"}, nil
}
