package media

import (
	"encoding/binary"
	"io"
	"time"
)

// MP4/QuickTime metadata, read directly from the container's box tree.
//
// The comment this replaces said the honest options were a cgo dependency on
// ffmpeg or shelling out to it. That is true for a THUMBNAIL, which needs a
// video decoder and an inter-frame codec, and it stays true — Render still
// declines video. It is not true for metadata: duration, dimensions, rotation
// and capture time are plain fields in the `moov` header, ahead of any codec, so
// reading them is parsing a length-prefixed tree, not decoding video. That is
// worth doing in-process precisely because it does NOT drag in the dependency
// the thumbnail would.
//
// Everything here is bounds-checked against the buffer it was handed and never
// seeks, allocates per box, or trusts a declared length. A container is
// attacker-supplied input on the same footing as an image, and this parser sits
// in the same job that already treats decoding as hostile.

// mp4Boxes that matter, and the shape of the ones we read:
//
//	moov                     the metadata container
//	  mvhd                   timescale + duration for the whole presentation
//	  trak                   one per stream
//	    tkhd                 per-track dimensions and the display matrix
//
// mdat — the actual media — is skipped by length and never read.

// maxBoxDepth bounds nesting. Real files are three or four deep; anything
// claiming more is either corrupt or trying to make the parser recurse until the
// stack gives out.
const maxBoxDepth = 8

// mp4EpochOffset converts QuickTime time (seconds since 1904-01-01 UTC) to Unix
// time. Videos frequently store 0 here, which means "unset" rather than 1904.
const mp4EpochOffset = 2082844800

// looksLikeMP4 reports whether the buffer begins with an ISO base media file.
// The first box of an MP4 or MOV is `ftyp`, except for some older QuickTime
// files that lead with `moov`, `mdat`, `free`, `skip` or `wide`.
func looksLikeMP4(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	switch string(data[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide":
		return true
	}
	return false
}

// parseMP4 fills what the box tree can tell us. It reports ok=false when the
// buffer holds no `moov` — which is not corruption and not rare: a file that was
// not written "faststart" carries moov after the media data, so a large
// recording's header can sit beyond the prefix this package is handed. The
// caller degrades to recording that the file is a video, exactly as before.
func parseMP4(data []byte) (Metadata, bool) {
	moov, found := findBox(data, "moov", 0)
	if !found {
		return Metadata{}, false
	}
	return parseMoov(moov)
}

// parseMoov reads one `moov` body, whichever way it was located — from the
// prefix already in memory, or from the seek walk below. Split out so both
// paths produce identical metadata by construction rather than by two
// implementations agreeing.
func parseMoov(moov []byte) (Metadata, bool) {
	m := Metadata{Orientation: 1, Source: "video"}
	ok := false

	if mvhd, found := findBox(moov, "mvhd", 0); found {
		if d, created, good := parseMVHD(mvhd); good {
			if d > 0 {
				ms := d
				m.DurationMS = &ms
			}
			if created != nil {
				m.TakenAt = created
			}
			ok = true
		}
	}

	// The largest track wins. A recording carries at least a video and an audio
	// track, and an audio tkhd is a legitimate 0x0 — picking by area rather than
	// by order avoids depending on which the muxer wrote first.
	best := 0
	forEachBox(moov, func(typ string, body []byte) {
		if typ != "trak" {
			return
		}
		tkhd, found := findBox(body, "tkhd", 0)
		if !found {
			return
		}
		w, h, orientation, good := parseTKHD(tkhd)
		if !good || w <= 0 || h <= 0 || w*h <= best {
			return
		}
		best = w * h
		m.Width, m.Height = w, h
		m.Orientation = orientation
		ok = true
	})

	return m, ok
}

// maxMoovBytes bounds how much of a trailing `moov` will be read into memory.
//
// A moov carries the sample tables, which grow with the length of the
// recording rather than with its resolution: an hour of 60fps video is on the
// order of a megabyte of them. 64 MiB is far above any real file and far below
// the point where one crafted header can take the worker's memory — the same
// shape of bound MaxInputBytes is, applied to the one read that is allowed to
// reach past the prefix.
const maxMoovBytes = 64 << 20

// maxTopLevelBoxes bounds how many boxes the seek walk will step through
// looking for `moov`. A real file has a handful — ftyp, free, mdat, moov — and
// a file with thousands is one whose header is being used to make us do
// unbounded IO one 8-byte read at a time.
const maxTopLevelBoxes = 256

// parseMP4Seek finds `moov` in a file too large to hold in memory, by walking
// the top-level boxes with the seeker rather than scanning bytes.
//
// This is the fix for the case analyzeVideo used to simply record as "a video,
// nothing known": a recording that was not written "faststart" carries moov
// AFTER the media data, so on any file longer than the bounded prefix the
// header was genuinely unreachable. It is unreachable from a PREFIX, not from
// the file — and the worker's opener hands back an io.ReadSeekCloser, because
// the download path needs Range support anyway.
//
// The walk is cheap and bounded: each step reads 8 or 16 bytes of header and
// jumps by the declared length, so finding a trailing moov in a 4 GB file costs
// a handful of seeks rather than 4 GB of reads. mdat is never read.
//
// It reports ok=false for anything it cannot resolve — an unseekable source, a
// truncated file, a moov larger than the bound — and the caller degrades to the
// bare video record exactly as before. What it must never do is report metadata
// it did not actually read.
func parseMP4Seek(rs io.ReadSeeker) (Metadata, bool) {
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil || size < 8 {
		return Metadata{}, false
	}

	var hdr [16]byte
	for i, off := 0, int64(0); i < maxTopLevelBoxes && off+8 <= size; i++ {
		if _, err := rs.Seek(off, io.SeekStart); err != nil {
			return Metadata{}, false
		}
		if _, err := io.ReadFull(rs, hdr[:8]); err != nil {
			return Metadata{}, false
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		header := int64(8)

		switch boxSize {
		case 0:
			// "to end of file" — the last box, so its extent is known exactly
			// here in a way it never is from a prefix.
			boxSize = size - off
		case 1:
			if _, err := io.ReadFull(rs, hdr[8:16]); err != nil {
				return Metadata{}, false
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			header = 16
		}
		if boxSize < header || off+boxSize > size {
			return Metadata{}, false // nonsense, or a truncated file
		}

		if typ == "moov" {
			body := boxSize - header
			if body <= 0 || body > maxMoovBytes {
				return Metadata{}, false
			}
			buf := make([]byte, body)
			if _, err := rs.Seek(off+header, io.SeekStart); err != nil {
				return Metadata{}, false
			}
			if _, err := io.ReadFull(rs, buf); err != nil {
				return Metadata{}, false
			}
			return parseMoov(buf)
		}
		off += boxSize
	}
	return Metadata{}, false
}

// forEachBox walks the boxes laid out directly in data, handing each one its
// body. It stops at the first malformed length rather than guessing, because
// the remainder of a truncated tree is not meaningfully parseable.
func forEachBox(data []byte, fn func(typ string, body []byte)) {
	for off := 0; off+8 <= len(data); {
		size := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		header := int64(8)

		switch size {
		case 0:
			// "to end of file" — the last box.
			fn(typ, data[off+8:])
			return
		case 1:
			// 64-bit length follows the type.
			if off+16 > len(data) {
				return
			}
			size = int64(binary.BigEndian.Uint64(data[off+8:]))
			header = 16
		}

		if size < header || off+int(size) > len(data) {
			// Either nonsense, or a box whose body runs past what we were given
			// — an mdat larger than the prefix, most often. Nothing after it can
			// be located, so stop.
			return
		}
		fn(typ, data[off+int(header):off+int(size)])
		off += int(size)
	}
}

// findBox looks for one box type among data's children, descending into
// containers. Depth-bounded; returns the body.
func findBox(data []byte, want string, depth int) ([]byte, bool) {
	if depth > maxBoxDepth {
		return nil, false
	}
	var (
		out   []byte
		found bool
	)
	forEachBox(data, func(typ string, body []byte) {
		if found {
			return
		}
		if typ == want {
			out, found = body, true
			return
		}
		// Only descend into the containers on the path to what we read. Walking
		// every box would mean walking sample tables, which are large and have
		// nothing we want.
		switch typ {
		case "trak", "mdia", "minf", "stbl", "edts":
			if b, ok := findBox(body, want, depth+1); ok {
				out, found = b, true
			}
		}
	})
	return out, found
}

// parseMVHD reads the movie header: duration in milliseconds, and the creation
// time if the muxer set one.
func parseMVHD(b []byte) (durationMS int64, created *time.Time, ok bool) {
	if len(b) < 4 {
		return 0, nil, false
	}
	version := b[0]

	var (
		createdRaw uint64
		timescale  uint32
		duration   uint64
	)
	switch version {
	case 0:
		if len(b) < 20 {
			return 0, nil, false
		}
		createdRaw = uint64(binary.BigEndian.Uint32(b[4:]))
		timescale = binary.BigEndian.Uint32(b[12:])
		duration = uint64(binary.BigEndian.Uint32(b[16:]))
	case 1:
		if len(b) < 32 {
			return 0, nil, false
		}
		createdRaw = binary.BigEndian.Uint64(b[4:])
		timescale = binary.BigEndian.Uint32(b[20:])
		duration = binary.BigEndian.Uint64(b[24:])
	default:
		return 0, nil, false
	}

	if timescale == 0 {
		// A zero timescale would be a divide by zero, and there is no sensible
		// default: without it the duration field has no unit.
		return 0, mp4Time(createdRaw), true
	}
	// 0xFFFFFFFF is the conventional "unknown duration" for a live or still-being
	// written file.
	if duration == 0 || duration == 0xFFFFFFFF {
		return 0, mp4Time(createdRaw), true
	}
	return int64(duration * 1000 / uint64(timescale)), mp4Time(createdRaw), true
}

// mp4Time converts a QuickTime timestamp, treating 0 as absent.
func mp4Time(raw uint64) *time.Time {
	if raw == 0 || raw < mp4EpochOffset {
		return nil
	}
	t := time.Unix(int64(raw)-mp4EpochOffset, 0).UTC()
	return &t
}

// parseTKHD reads a track header: its display dimensions and its rotation.
//
// width and height are 16.16 fixed point and are the DISPLAY size — already the
// dimensions a player lays out, before the matrix is applied.
func parseTKHD(b []byte) (width, height, orientation int, ok bool) {
	if len(b) < 4 {
		return 0, 0, 0, false
	}

	// Everything before the matrix is fixed-size but version-dependent.
	var offset int
	switch b[0] {
	case 0:
		offset = 4 + 4 + 4 + 4 + 4 + 4 // version/flags, created, modified, id, reserved, duration
	case 1:
		offset = 4 + 8 + 8 + 4 + 4 + 8
	default:
		return 0, 0, 0, false
	}
	// reserved(8) + layer(2) + alternate_group(2) + volume(2) + reserved(2)
	offset += 16

	matrix := offset
	if matrix+36+8 > len(b) {
		return 0, 0, 0, false
	}
	w := int(binary.BigEndian.Uint32(b[matrix+36:]) >> 16)
	h := int(binary.BigEndian.Uint32(b[matrix+40:]) >> 16)

	return w, h, matrixOrientation(b[matrix : matrix+36]), true
}

// matrixOrientation maps the track's 3x3 display matrix onto the EXIF
// orientation flag the rest of the system already speaks, so a rotated phone
// video and a rotated photo are described the same way and one viewer handles
// both.
//
// Only the four quarter-turns are recognised. A matrix doing anything else —
// a flip, a shear, a scale — is reported as "as stored" rather than guessed at:
// EXIF orientation cannot express it, and inventing the nearest rotation would
// display the video wrongly with confidence.
func matrixOrientation(m []byte) int {
	if len(m) < 36 {
		return 1
	}
	// a and b are the first row, c and d the second, each 16.16 fixed point.
	a := int32(binary.BigEndian.Uint32(m[0:]))
	b := int32(binary.BigEndian.Uint32(m[4:]))
	c := int32(binary.BigEndian.Uint32(m[12:]))
	d := int32(binary.BigEndian.Uint32(m[16:]))

	const one = 1 << 16
	switch {
	case a == one && b == 0 && c == 0 && d == one:
		return 1 // 0°
	case a == 0 && b == one && c == -one && d == 0:
		return 6 // 90° clockwise
	case a == -one && b == 0 && c == 0 && d == -one:
		return 3 // 180°
	case a == 0 && b == -one && c == one && d == 0:
		return 8 // 270° clockwise
	default:
		return 1
	}
}
