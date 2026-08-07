package media

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"
)

// A deliberately small EXIF reader.
//
// It reads six things — orientation, capture time, camera make/model and GPS —
// and ignores the rest of the specification. That is the whole requirement: a
// timeline sorts by capture time, a viewer needs the rotation flag, and a map
// needs coordinates. Nothing here wants a general-purpose metadata library.
//
// Hand-rolled rather than taken as a dependency because the alternative is
// pulling an unmaintained package into the parser that will be pointed at every
// image a user uploads. This file is ~200 lines, has no allocations proportional
// to attacker-controlled counts, and every offset it follows is bounds-checked
// against the buffer it came from. An EXIF block is untrusted input in the most
// literal sense: it arrives from a stranger's camera, or from someone who wants
// to see what a malformed one does.

var errNoEXIF = errors.New("no exif block")

// EXIF tag ids, from the TIFF/EXIF specification.
const (
	tagMake            = 0x010F
	tagModel           = 0x0110
	tagOrientation     = 0x0112
	tagExifIFD         = 0x8769
	tagGPSIFD          = 0x8825
	tagDateTimeOrig    = 0x9003
	tagDateTimeDigital = 0x9004
	tagOffsetTimeOrig  = 0x9011

	gpsLatRef = 0x0001
	gpsLat    = 0x0002
	gpsLonRef = 0x0003
	gpsLon    = 0x0004
)

// TIFF field types, only the ones these tags actually use.
const (
	typeASCII    = 2
	typeShort    = 3
	typeLong     = 4
	typeRational = 5
)

// Bounds on what will be walked. An IFD claiming a million entries is either
// corrupt or hostile; either way it gets refused rather than served.
const (
	maxIFDEntries = 512
	maxIFDDepth   = 4
)

// exifData is the raw EXIF block plus its byte order.
type exifData struct {
	buf   []byte // starts at the TIFF header, which all offsets are relative to
	order binary.ByteOrder
}

// findEXIF locates the EXIF block in a JPEG and returns a reader over it.
//
// JPEG only. PNG and GIF have no EXIF worth the name, and WebP's is rare enough
// that the absence costs a rotation flag rather than a feature.
func findEXIF(data []byte) (*exifData, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errNoEXIF // not a JPEG
	}

	// Walk the marker segments looking for APP1/Exif. Every length is checked
	// against the remaining buffer before it is used to advance.
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return nil, errNoEXIF // desynchronised; refuse rather than guess
		}
		marker := data[i+1]
		// Standalone markers carry no payload.
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		// Start of scan: image data follows, no more metadata.
		if marker == 0xDA || marker == 0xD9 {
			return nil, errNoEXIF
		}
		if i+4 > len(data) {
			return nil, errNoEXIF
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2:]))
		if segLen < 2 || i+2+segLen > len(data) {
			return nil, errNoEXIF
		}
		if marker == 0xE1 {
			payload := data[i+4 : i+2+segLen]
			if len(payload) > 6 && string(payload[:6]) == "Exif\x00\x00" {
				return newEXIF(payload[6:])
			}
		}
		i += 2 + segLen
	}
	return nil, errNoEXIF
}

// newEXIF validates the TIFF header that every EXIF block begins with.
func newEXIF(buf []byte) (*exifData, error) {
	if len(buf) < 8 {
		return nil, errNoEXIF
	}
	var order binary.ByteOrder
	switch {
	case buf[0] == 'I' && buf[1] == 'I':
		order = binary.LittleEndian
	case buf[0] == 'M' && buf[1] == 'M':
		order = binary.BigEndian
	default:
		return nil, errNoEXIF
	}
	if order.Uint16(buf[2:]) != 42 { // the TIFF magic number
		return nil, errNoEXIF
	}
	return &exifData{buf: buf, order: order}, nil
}

// entry is one parsed IFD field.
type entry struct {
	tag    uint16
	typ    uint16
	count  uint32
	offset uint32 // offset into buf, already resolved past the inline case
	inline []byte // the raw 4 value bytes, for values that fit
}

// walk visits every entry in the IFD at off, calling fn for each.
//
// depth bounds pointer chasing: IFDs reference sub-IFDs (Exif, GPS), and a
// crafted file can make them reference each other in a cycle. Without the bound
// that is an infinite loop in a worker.
func (e *exifData) walk(off uint32, depth int, fn func(entry)) {
	if depth > maxIFDDepth || int(off)+2 > len(e.buf) {
		return
	}
	count := int(e.order.Uint16(e.buf[off:]))
	if count > maxIFDEntries {
		count = maxIFDEntries
	}
	pos := int(off) + 2
	for i := 0; i < count; i++ {
		if pos+12 > len(e.buf) {
			return
		}
		raw := e.buf[pos : pos+12]
		en := entry{
			tag:    e.order.Uint16(raw[0:]),
			typ:    e.order.Uint16(raw[2:]),
			count:  e.order.Uint32(raw[4:]),
			inline: raw[8:12],
		}
		en.offset = e.order.Uint32(raw[8:])
		fn(en)

		// Sub-IFDs are followed here rather than by the caller, so the depth
		// bound applies to them too.
		if en.tag == tagExifIFD || en.tag == tagGPSIFD {
			e.walk(en.offset, depth+1, fn)
		}
		pos += 12
	}
}

// valueBytes returns an entry's raw value, resolving the inline-vs-offset rule:
// a value of four bytes or fewer is stored in the entry itself.
func (e *exifData) valueBytes(en entry, size int) []byte {
	n := int(en.count) * size
	if n <= 0 {
		return nil
	}
	if n <= 4 {
		return en.inline[:n]
	}
	if int(en.offset) < 0 || int(en.offset)+n > len(e.buf) {
		return nil // an offset past the end of the block: ignore, never panic
	}
	return e.buf[en.offset : int(en.offset)+n]
}

func (e *exifData) ascii(en entry) string {
	if en.typ != typeASCII {
		return ""
	}
	b := e.valueBytes(en, 1)
	// EXIF strings are NUL-terminated and often NUL-padded.
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

func (e *exifData) short(en entry) (uint16, bool) {
	if en.typ != typeShort || en.count == 0 {
		return 0, false
	}
	b := e.valueBytes(en, 2)
	if len(b) < 2 {
		return 0, false
	}
	return e.order.Uint16(b), true
}

// rationals reads a RATIONAL array — the form GPS coordinates take, as three
// numerator/denominator pairs for degrees, minutes and seconds.
func (e *exifData) rationals(en entry, want int) []float64 {
	if en.typ != typeRational || int(en.count) < want {
		return nil
	}
	b := e.valueBytes(en, 8)
	if len(b) < want*8 {
		return nil
	}
	out := make([]float64, want)
	for i := 0; i < want; i++ {
		num := e.order.Uint32(b[i*8:])
		den := e.order.Uint32(b[i*8+4:])
		if den == 0 {
			return nil // a zero denominator is corrupt, not zero degrees
		}
		out[i] = float64(num) / float64(den)
	}
	return out
}

// parseEXIF pulls the fields this system uses out of a JPEG's EXIF block.
// Every field is optional; a file with no EXIF is not an error.
func parseEXIF(data []byte) Metadata {
	var m Metadata
	e, err := findEXIF(data)
	if err != nil {
		return m
	}
	if len(e.buf) < 8 {
		return m
	}
	ifd0 := e.order.Uint32(e.buf[4:])

	var (
		dateOrig, dateDigital, offsetTime string
		latRef, lonRef                    string
		latVals, lonVals                  []float64
		make_, model                      string
	)

	e.walk(ifd0, 0, func(en entry) {
		switch en.tag {
		case tagOrientation:
			if v, ok := e.short(en); ok && v >= 1 && v <= 8 {
				m.Orientation = int(v)
			}
		case tagMake:
			make_ = e.ascii(en)
		case tagModel:
			model = e.ascii(en)
		case tagDateTimeOrig:
			dateOrig = e.ascii(en)
		case tagDateTimeDigital:
			dateDigital = e.ascii(en)
		case tagOffsetTimeOrig:
			offsetTime = e.ascii(en)
		case gpsLatRef:
			if s := e.ascii(en); s != "" {
				latRef = s
			}
		case gpsLonRef:
			if s := e.ascii(en); s != "" {
				lonRef = s
			}
		case gpsLat:
			latVals = e.rationals(en, 3)
		case gpsLon:
			lonVals = e.rationals(en, 3)
		}
	})

	m.Camera = joinCamera(make_, model)

	// DateTimeOriginal is when the shutter fired; DateTimeDigitized is when it
	// was written to card. Prefer the former and fall back, because a scanned
	// photo has only the latter.
	if t, ok := parseEXIFTime(dateOrig, offsetTime); ok {
		m.TakenAt = &t
	} else if t, ok := parseEXIFTime(dateDigital, offsetTime); ok {
		m.TakenAt = &t
	}

	if lat, ok := toDegrees(latVals, latRef, "N", "S"); ok {
		if lon, ok := toDegrees(lonVals, lonRef, "E", "W"); ok {
			// Reject the null island. A camera that failed to get a fix writes
			// exactly 0,0 far more often than anyone photographs that spot in
			// the Atlantic, and a map pin there is worse than no pin.
			if lat != 0 || lon != 0 {
				m.GPSLat, m.GPSLon = &lat, &lon
			}
		}
	}
	return m
}

// joinCamera renders "Make Model" without repeating the make when the model
// already contains it — "Canon" + "Canon EOS R6" should not become
// "Canon Canon EOS R6", which is what most cameras actually write.
func joinCamera(make_, model string) string {
	make_, model = strings.TrimSpace(make_), strings.TrimSpace(model)
	switch {
	case make_ == "" && model == "":
		return ""
	case make_ == "":
		return model
	case model == "":
		return make_
	case strings.HasPrefix(strings.ToLower(model), strings.ToLower(make_)):
		return model
	default:
		return make_ + " " + model
	}
}

// parseEXIFTime reads EXIF's "2006:01:02 15:04:05" form.
//
// EXIF timestamps carry no zone. OffsetTimeOriginal supplies one when present
// (it is a 2003 addition and most files still lack it); without it the time is
// read as UTC. That is a documented lie, but it is the conventional one: the
// alternative — the server's local zone — makes the same photo sort differently
// depending on which machine indexed it.
func parseEXIFTime(s, offset string) (time.Time, bool) {
	s = strings.TrimSpace(strings.TrimRight(s, "\x00"))
	if s == "" {
		return time.Time{}, false
	}
	if offset = strings.TrimSpace(offset); offset != "" {
		if t, err := time.Parse("2006:01:02 15:04:05-07:00", s+offset); err == nil {
			return t.UTC(), true
		}
	}
	t, err := time.Parse("2006:01:02 15:04:05", s)
	if err != nil {
		return time.Time{}, false
	}
	// A zero or absurd year means the camera's clock was never set. Better no
	// date than a timeline anchored in 1970.
	if t.Year() < 1900 || t.Year() > 2200 {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// toDegrees converts EXIF's degrees/minutes/seconds triple to a signed decimal.
func toDegrees(v []float64, ref, pos, neg string) (float64, bool) {
	if len(v) != 3 {
		return 0, false
	}
	d := v[0] + v[1]/60 + v[2]/3600
	if math.IsNaN(d) || math.IsInf(d, 0) || d > 180 {
		return 0, false
	}
	switch strings.ToUpper(strings.TrimSpace(ref)) {
	case neg:
		return -d, true
	case pos:
		return d, true
	default:
		return 0, false // an absent or unrecognised hemisphere is unusable
	}
}
