// Package extract pulls searchable text out of files: a plain decode for text,
// tesseract OCR for images, a text-layer read for PDFs.
//
// It is pure of the database and the queue — it takes bytes and a content type
// and returns text — so the worker can wrap it and a test can call it directly.
// OCR shells out to the `tesseract` binary as a subprocess: no cgo, and a crash
// in the C library takes the subprocess, not the worker. Where tesseract is not
// installed, image extraction reports that cleanly rather than failing, so a
// deployment without OCR still runs.
package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

var (
	// ErrUnsupported means this content type has no extractor — a video, an
	// archive. Not an error to retry; there is simply nothing to extract.
	ErrUnsupported = errors.New("no extractor for this content type")
	// ErrNoText means an extractor ran but found nothing worth indexing — an image
	// of a blank wall, a scanned PDF with no text layer and no OCR available.
	ErrNoText = errors.New("no extractable text found")
	// ErrNoOCR means an image needs OCR but tesseract is not installed. A
	// deployment can legitimately choose not to run OCR; the worker treats this as
	// "skip", not "fail".
	ErrNoOCR = errors.New("image needs OCR but tesseract is unavailable")
)

const (
	// MaxInputBytes caps what will be read into memory to extract from. A
	// maliciously enormous file must not let one job occupy the worker unbounded.
	MaxInputBytes = 64 << 20 // 64 MiB
	// MaxTextBytes caps stored text. Beyond this, more text does not make a
	// document more findable, and the row and its trigram index would just grow.
	MaxTextBytes = 1 << 20 // 1 MiB
)

// Result is extracted text and how it was obtained.
type Result struct {
	Text   string
	Lang   string
	Source string // "text", "ocr", or "pdf"
}

// Extractor extracts text. It is safe for concurrent use; each call is
// independent, and the tesseract path is resolved once at construction.
type Extractor struct {
	tesseract string        // resolved binary path, or "" if unavailable
	lang      string        // tesseract language(s), e.g. "eng"
	timeout   time.Duration // per-extraction wall-clock cap
}

// New builds an Extractor, resolving tesseract if it is on PATH.
func New() *Extractor {
	path, _ := exec.LookPath("tesseract")
	return &Extractor{tesseract: path, lang: "eng", timeout: 2 * time.Minute}
}

// HasOCR reports whether OCR is available in this environment.
func (e *Extractor) HasOCR() bool { return e.tesseract != "" }

// Extract reads up to MaxInputBytes from r and returns extracted text, chosen by
// content type. The returned error is one of the sentinels above for the
// "nothing to do" cases, which the caller records as a completed job rather than
// a failure to retry.
func (e *Extractor) Extract(ctx context.Context, contentType string, r io.Reader) (Result, error) {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	data, err := io.ReadAll(io.LimitReader(r, MaxInputBytes))
	if err != nil {
		return Result{}, fmt.Errorf("read input: %w", err)
	}

	switch {
	case isTextual(mediaType):
		return e.extractText(data)
	case mediaType == "application/pdf":
		return e.extractPDF(data)
	case strings.HasPrefix(mediaType, "image/"):
		return e.extractImage(ctx, data)
	default:
		return Result{}, ErrUnsupported
	}
}

// extractText decodes a text file. Invalid UTF-8 is rejected rather than indexed:
// a binary file mislabelled as text/plain would otherwise fill the index with
// mojibake that matches nothing a person would search for.
func (e *Extractor) extractText(data []byte) (Result, error) {
	if !utf8.Valid(data) {
		// ErrNoText, not ErrUnsupported: an extractor DID run and this content
		// type IS supported — there is simply nothing here worth indexing. The
		// handler treats both as "job complete", but the distinction is what the
		// two sentinels are for, and a future caller that tells them apart (a
		// "why was this not indexed?" view, say) would otherwise be told this
		// file's type has no extractor, which is false.
		return Result{}, ErrNoText
	}
	text := clean(string(data))
	if text == "" {
		return Result{}, ErrNoText
	}
	return Result{Text: truncateText(text), Source: "text"}, nil
}

// extractPDF reads a PDF's text layer. It does not render scanned pages to images
// for OCR — that needs a heavier toolchain — so a scanned PDF with no text layer
// reports ErrNoText. The library can panic on a malformed file, so the read is
// guarded: a bad PDF is a skip, never a worker crash.
func (e *Extractor) extractPDF(data []byte) (result Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result, err = Result{}, ErrNoText
		}
	}()

	r, perr := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if perr != nil {
		return Result{}, ErrNoText
	}
	reader, perr := r.GetPlainText()
	if perr != nil {
		return Result{}, ErrNoText
	}
	buf, perr := io.ReadAll(io.LimitReader(reader, MaxTextBytes*2))
	if perr != nil {
		return Result{}, ErrNoText
	}
	text := clean(string(buf))
	if text == "" {
		return Result{}, ErrNoText
	}
	return Result{Text: truncateText(text), Source: "pdf"}, nil
}

// extractImage OCRs an image via tesseract. The image is written to a temp file
// because tesseract's stdin handling varies across versions, while a file
// argument is universal.
func (e *Extractor) extractImage(ctx context.Context, data []byte) (Result, error) {
	if e.tesseract == "" {
		return Result{}, ErrNoOCR
	}

	tmp, err := os.CreateTemp("", "pc-ocr-*")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Result{}, err
	}
	tmp.Close()

	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// `tesseract <input> stdout -l eng`: OCR the file and write plain text to
	// stdout rather than to an output file we would then have to read and clean up.
	cmd := exec.CommandContext(ctx, e.tesseract, tmp.Name(), "stdout", "-l", e.lang)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("ocr timed out: %w", ctx.Err())
		}
		return Result{}, fmt.Errorf("tesseract: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}

	text := clean(out.String())
	if text == "" {
		return Result{}, ErrNoText
	}
	return Result{Text: truncateText(text), Lang: e.lang, Source: "ocr"}, nil
}

// isTextual reports whether a media type is plain text we can decode directly.
func isTextual(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/javascript",
		"application/x-ndjson", "application/csv", "application/yaml",
		"application/x-yaml", "application/toml":
		return true
	}
	// A structured-text convention: anything +xml / +json (e.g. image/svg+xml).
	return strings.HasSuffix(mediaType, "+xml") || strings.HasSuffix(mediaType, "+json")
}

// clean normalizes extracted text: trims, and drops NUL bytes that a mislabelled
// binary would carry and that Postgres rejects in a text column outright.
func clean(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(s)
}

// truncateText bounds stored text on a rune boundary, so truncation never splits
// a multibyte character into invalid UTF-8.
func truncateText(s string) string {
	if len(s) <= MaxTextBytes {
		return s
	}
	s = s[:MaxTextBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
