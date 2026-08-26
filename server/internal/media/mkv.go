package media

import (
	"encoding/binary"
	"math"
	"time"
)

// Matroska and WebM metadata, read directly from the EBML tree.
//
// Same argument mp4.go makes, and the same limit. Duration, dimensions,
// rotation and capture time are plain elements near the FRONT of a Matroska
// file — the spec requires Info and Tracks before the first Cluster, because a
// player cannot start without them — so reading them is walking a
// length-prefixed tree, not decoding video. A thumbnail still needs a decoder
// and still lives behind the ffmpeg switch in video.go.
//
// EBML is the same shape as an ISO box tree with a different encoding: instead
// of a fixed 4-byte type and 4-byte length, both the id and the length are
// variable-width integers whose leading bits give their own width. Everything
// below is bounds-checked against the buffer it was handed, never seeks, and
// never trusts a declared length — a container is attacker-supplied input on
// exactly the same footing as an image.
//
// One deliberate gap: some muxers record rotation as a `ROTATE` SimpleTag in
// the Tags element rather than in the Video element's Projection. Tags are
// usually written at the END of the file, past the bounded prefix this package
// reads, so honouring them would be an inconsistent promise — sometimes read,
// sometimes not, with no way for a caller to tell which. Projection is read
// because it sits in Tracks, which is always at the front.

// The element ids that matter. An EBML id carries its own length marker, so
// these are the on-the-wire bytes, not stripped values — which is what makes a
// single uint32 comparison enough to identify one.
const (
	idEBMLHeader         = 0x1A45DFA3
	idSegment            = 0x18538067
	idInfo               = 0x1549A966
	idTimestampScale     = 0x2AD7B1
	idDuration           = 0x4489
	idDateUTC            = 0x4461
	idTracks             = 0x1654AE6B
	idTrackEntry         = 0xAE
	idTrackType          = 0x83
	idVideo              = 0xE0
	idPixelWidth         = 0xB0
	idPixelHeight        = 0xBA
	idDisplayWidth       = 0x54B0
	idDisplayHeight      = 0x54BA
	idProjection         = 0x7670
	idProjectionPoseRoll = 0x7675
)

// trackTypeVideo is the TrackType value for a video track. Audio is 2,
// subtitles 17; a file carries several tracks and only one of them has
// dimensions worth reporting.
const trackTypeVideo = 1

// maxEBMLDepth bounds nesting, for the reason maxBoxDepth does: Segment → Tracks
// → TrackEntry → Video → Projection is five, and anything claiming materially
// more is either corrupt or trying to recurse the stack away.
const maxEBMLDepth = 8

// defaultTimestampScale is what the spec says an absent TimestampScale means:
// one millisecond, expressed in nanoseconds. Duration is a count of these
// ticks, so getting the default wrong scales every duration by a thousand.
const defaultTimestampScale = 1_000_000

// mkvEpochOffset converts Matroska time (nanoseconds since 2001-01-01 UTC) to
// Unix seconds. A different epoch from both Unix and QuickTime, for no reason
// anybody records.
const mkvEpochOffset = 978307200

// looksLikeMatroska reports whether the buffer begins with an EBML header.
// Matroska and WebM are the same container and share this magic; only the
// DocType inside distinguishes them, and nothing here needs to care which.
func looksLikeMatroska(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	return binary.BigEndian.Uint32(data[:4]) == idEBMLHeader
}

// parseMKV fills what the EBML tree can tell us, reporting ok=false when the
// buffer holds no usable Segment.
//
// Unlike MP4 there is no "header written last" case to degrade for: Matroska
// requires Info and Tracks ahead of the first Cluster, so a bounded prefix that
// contains the Segment header contains everything read here. A false return
// therefore means the file is truncated or not really Matroska, and the caller
// degrades to the bare video record exactly as it does for MP4.
func parseMKV(data []byte) (Metadata, bool) {
	seg, found := findEBML(data, idSegment, 0)
	if !found {
		return Metadata{}, false
	}

	m := Metadata{Orientation: 1, Source: "video"}
	ok := false

	if info, found := findEBML(seg, idInfo, 0); found {
		scale := int64(defaultTimestampScale)
		var ticks float64
		forEachEBML(info, func(id uint32, body []byte) {
			switch id {
			case idTimestampScale:
				// A zero scale would make every duration zero, which is not a
				// duration this file has — treat it as absent and keep the default.
				if v, good := ebmlUint(body); good && v > 0 {
					scale = int64(v)
				}
			case idDuration:
				if v, good := ebmlFloat(body); good {
					ticks = v
				}
			case idDateUTC:
				if t := mkvTime(body); t != nil {
					m.TakenAt = t
				}
			}
		})
		// Computed after the walk rather than inside it: TimestampScale is
		// allowed to appear after Duration, and multiplying by a default that
		// was about to be overwritten is off by whatever factor the file chose.
		if ticks > 0 && scale > 0 {
			if ms := int64(ticks * float64(scale) / 1e6); ms > 0 {
				m.DurationMS = &ms
			}
		}
		ok = true
	}

	// The largest video track wins, for the reason the MP4 parser picks by area:
	// a file carries several tracks, and depending on the order the muxer wrote
	// them, "the first one" is as likely to be audio as video.
	best := 0
	forEachEBML(seg, func(id uint32, body []byte) {
		if id != idTracks {
			return
		}
		forEachEBML(body, func(id uint32, entry []byte) {
			if id != idTrackEntry {
				return
			}
			// TrackType is mandatory, but a file that omits it still has a Video
			// element or it does not, and that is the test that actually matters.
			if raw, found := findEBML(entry, idTrackType, maxEBMLDepth); found {
				if v, good := ebmlUint(raw); good && v != trackTypeVideo {
					return
				}
			}
			video, found := findEBML(entry, idVideo, maxEBMLDepth)
			if !found {
				return
			}
			w, h, orientation := parseMKVVideo(video)
			if w <= 0 || h <= 0 || w*h <= best {
				return
			}
			best = w * h
			m.Width, m.Height = w, h
			m.Orientation = orientation
			ok = true
		})
	})

	return m, ok
}

// parseMKVVideo reads one Video element: the dimensions a player lays out and
// the rotation it applies first.
//
// DisplayWidth/DisplayHeight win over PixelWidth/PixelHeight where present,
// because they are what a player actually renders — an anamorphic recording
// stores 720 pixels across and displays 1024, and storing the encoded number
// would make the gallery lay the tile out at the wrong aspect ratio. This
// matches the MP4 path, where tkhd's dimensions are already display dimensions.
func parseMKVVideo(video []byte) (width, height, orientation int) {
	orientation = 1
	var px, py, dx, dy uint64
	forEachEBML(video, func(id uint32, body []byte) {
		switch id {
		case idPixelWidth:
			if v, good := ebmlUint(body); good {
				px = v
			}
		case idPixelHeight:
			if v, good := ebmlUint(body); good {
				py = v
			}
		case idDisplayWidth:
			if v, good := ebmlUint(body); good {
				dx = v
			}
		case idDisplayHeight:
			if v, good := ebmlUint(body); good {
				dy = v
			}
		case idProjection:
			if raw, found := findEBML(body, idProjectionPoseRoll, maxEBMLDepth); found {
				if roll, good := ebmlFloat(raw); good {
					orientation = rollOrientation(roll)
				}
			}
		}
	})

	// Display dimensions are only usable as a pair: one of them alone alongside
	// a pixel dimension describes no rectangle either field meant.
	if dx > 0 && dy > 0 {
		return clampDimension(dx), clampDimension(dy), orientation
	}
	return clampDimension(px), clampDimension(py), orientation
}

// clampDimension turns an EBML unsigned into an int the media_meta column can
// hold, refusing anything absurd rather than wrapping. A declared width of 2^40
// is not a video, and an int that silently overflowed would be worse than none.
func clampDimension(v uint64) int {
	if v == 0 || v > 1<<20 {
		return 0
	}
	return int(v)
}

// rollOrientation maps ProjectionPoseRoll onto the EXIF orientation flag the
// rest of the system speaks, so a rotated Matroska recording, a rotated MP4 and
// a rotated photo are all described one way and one viewer handles all three.
//
// The convention taken here is that roll is the clockwise rotation to APPLY FOR
// DISPLAY — the same thing MP4's display matrix encodes, and the same thing
// EXIF orientation 6 means. The spec describes it as a camera pose, and real
// writers have disagreed about the sign of that, which is precisely why only
// exact quarter turns are honoured: an off-axis roll is reported as "as stored"
// rather than rounded to the nearest turn, because EXIF cannot express it and
// displaying a video confidently at the wrong angle is worse than not rotating
// it at all. Same refusal matrixOrientation makes for a flip or a shear.
func rollOrientation(roll float64) int {
	// Normalise into [0, 360) so -90 and 270 are the same turn, and tolerate the
	// float error a value that travelled through a 32-bit float carries.
	deg := math.Mod(roll, 360)
	if deg < 0 {
		deg += 360
	}
	const tolerance = 0.5
	switch {
	case math.Abs(deg-90) < tolerance:
		return 6
	case math.Abs(deg-180) < tolerance:
		return 3
	case math.Abs(deg-270) < tolerance:
		return 8
	default:
		return 1
	}
}

// mkvTime converts a DateUTC element, treating an absent or zero value as
// "the muxer did not set this" rather than as midnight on 2001-01-01.
func mkvTime(body []byte) *time.Time {
	ns, ok := ebmlInt(body)
	if !ok || ns == 0 {
		return nil
	}
	t := time.Unix(mkvEpochOffset+ns/1e9, ns%1e9).UTC()
	return &t
}

// forEachEBML walks the elements laid out directly in data, handing each one its
// body. Like forEachBox it stops at the first malformed length rather than
// guessing: past a bad width the remaining bytes are not a tree.
func forEachEBML(data []byte, fn func(id uint32, body []byte)) {
	for off := 0; off < len(data); {
		id, n, ok := ebmlID(data[off:])
		if !ok {
			return
		}
		off += n

		size, n, unknown, ok := ebmlSize(data[off:])
		if !ok {
			return
		}
		off += n

		if unknown {
			// Only the Segment is allowed to declare an unknown size here. A
			// live-muxed WebM legitimately does, and its body is the rest of what
			// we were handed. Any other element claiming it — a Cluster, most
			// likely — would make us walk frame data as if it were a tree, so we
			// stop instead.
			if id == idSegment {
				fn(id, data[off:])
			}
			return
		}
		if size < 0 || off+int(size) > len(data) {
			// Either nonsense, or an element running past the prefix we were
			// given. Nothing after it can be located, so stop.
			return
		}
		fn(id, data[off:off+int(size)])
		off += int(size)
	}
}

// findEBML looks for one element id among data's children, descending into the
// masters on the path to what we read. Depth-bounded; returns the body.
//
// Descending everywhere would mean walking Cluster payloads — the video itself —
// looking for a header field, which is both pointless and the one place a
// crafted file gets to choose how much work we do.
func findEBML(data []byte, want uint32, depth int) ([]byte, bool) {
	if depth > maxEBMLDepth {
		return nil, false
	}
	var (
		out   []byte
		found bool
	)
	forEachEBML(data, func(id uint32, body []byte) {
		if found {
			return
		}
		if id == want {
			out, found = body, true
			return
		}
		switch id {
		case idSegment, idTracks, idTrackEntry, idVideo, idProjection:
			if b, ok := findEBML(body, want, depth+1); ok {
				out, found = b, true
			}
		}
	})
	return out, found
}

// ebmlID reads a variable-width element id, keeping the length marker — the
// marker bits are part of the id as every table of them is written.
func ebmlID(data []byte) (id uint32, n int, ok bool) {
	if len(data) == 0 || data[0] == 0 {
		// A leading zero byte means a width of at least 5, which no id has.
		return 0, 0, false
	}
	width := 1
	for mask := byte(0x80); data[0]&mask == 0; mask >>= 1 {
		width++
	}
	if width > 4 || len(data) < width {
		return 0, 0, false
	}
	for i := 0; i < width; i++ {
		id = id<<8 | uint32(data[i])
	}
	return id, width, true
}

// ebmlSize reads a variable-width length, stripping the marker bit. A value
// whose every data bit is set means "unknown", which the caller handles.
func ebmlSize(data []byte) (size int64, n int, unknown, ok bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false, false
	}
	width := 1
	mask := byte(0x80)
	for data[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if width > 8 || len(data) < width {
		return 0, 0, false, false
	}
	// The marker bit sits at 1<<(8-width), so the first byte's data bits are
	// everything below it, and "all ones" for that byte means exactly mask-1.
	value := uint64(data[0] &^ mask)
	allOnes := value == uint64(mask)-1
	for i := 1; i < width; i++ {
		value = value<<8 | uint64(data[i])
		allOnes = allOnes && data[i] == 0xFF
	}
	if allOnes {
		return 0, width, true, true
	}
	// Sizes are compared against a buffer length, so anything that cannot be an
	// int64 offset is nonsense rather than a very large file.
	if value > math.MaxInt32 {
		return 0, width, false, false
	}
	return int64(value), width, false, true
}

// ebmlUint reads an unsigned integer element: 0 to 8 big-endian bytes, where an
// empty body means zero.
func ebmlUint(body []byte) (uint64, bool) {
	if len(body) > 8 {
		return 0, false
	}
	var v uint64
	for _, b := range body {
		v = v<<8 | uint64(b)
	}
	return v, true
}

// ebmlInt reads a signed integer element, sign-extending from its width.
func ebmlInt(body []byte) (int64, bool) {
	if len(body) == 0 || len(body) > 8 {
		return 0, false
	}
	v := int64(0)
	if body[0]&0x80 != 0 {
		v = -1 // sign-extend by starting from all ones
	}
	for _, b := range body {
		v = v<<8 | int64(b)
	}
	return v, true
}

// ebmlFloat reads a float element. EBML allows 0, 4 or 8 bytes; anything else
// is malformed rather than a float of some other precision.
func ebmlFloat(body []byte) (float64, bool) {
	switch len(body) {
	case 0:
		return 0, true
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(body))), true
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(body)), true
	default:
		return 0, false
	}
}
