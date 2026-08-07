package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegWithEXIF builds a real JPEG and splices an APP1/Exif segment in after SOI,
// so the tests exercise the actual marker walk rather than a hand-fed block.
func jpegWithEXIF(t *testing.T, w, h int, exif []byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if exif == nil {
		return raw
	}

	payload := append([]byte("Exif\x00\x00"), exif...)
	seg := make([]byte, 0, len(payload)+4)
	seg = append(seg, 0xFF, 0xE1)
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	seg = append(seg, payload...)

	out := make([]byte, 0, len(raw)+len(seg))
	out = append(out, raw[:2]...) // SOI
	out = append(out, seg...)
	out = append(out, raw[2:]...)
	return out
}

// field is one value to encode. Values of four bytes or fewer live inside the
// entry; anything longer goes on the heap and the entry holds its offset. The
// builder resolves that distinction so a test can just say what it wants.
type field struct {
	tag  uint16
	typ  uint16
	data []byte // the encoded value, already in little-endian
}

func ascii(tag uint16, s string) field {
	return field{tag: tag, typ: typeASCII, data: append([]byte(s), 0)}
}

func short(tag uint16, v uint16) field {
	return field{tag: tag, typ: typeShort, data: binary.LittleEndian.AppendUint16(nil, v)}
}

// rational encodes a degrees/minutes/seconds triple the way a GPS IFD does.
func rational(tag uint16, vals ...[2]uint32) field {
	var b []byte
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint32(b, v[0])
		b = binary.LittleEndian.AppendUint32(b, v[1])
	}
	return field{tag: tag, typ: typeRational, data: b}
}

// elemSize is the byte width of one element of a TIFF type, needed to turn a
// payload length back into the `count` the entry declares.
func elemSize(typ uint16) int {
	switch typ {
	case typeShort:
		return 2
	case typeLong:
		return 4
	case typeRational:
		return 8
	default:
		return 1
	}
}

// buildTIFF assembles a little-endian EXIF block with IFD0 plus an Exif and a
// GPS sub-IFD — the shape a camera actually writes.
func buildTIFF(ifd0, exif, gps []field) []byte {
	ifdBytes := func(n int) int { return 2 + 12*n + 4 }
	ifd0Off := 8
	exifOff := ifd0Off + ifdBytes(len(ifd0)+2) // +2 for the two sub-IFD pointers
	gpsOff := exifOff + ifdBytes(len(exif))
	heapBase := gpsOff + ifdBytes(len(gps))

	var heap []byte
	// encode writes one entry, putting its value inline or on the heap.
	encode := func(out *bytes.Buffer, f field) {
		binary.Write(out, binary.LittleEndian, f.tag)
		binary.Write(out, binary.LittleEndian, f.typ)
		binary.Write(out, binary.LittleEndian, uint32(len(f.data)/elemSize(f.typ)))
		if len(f.data) <= 4 {
			padded := make([]byte, 4)
			copy(padded, f.data)
			out.Write(padded)
			return
		}
		binary.Write(out, binary.LittleEndian, uint32(heapBase+len(heap)))
		heap = append(heap, f.data...)
	}

	pointer := func(tag uint16, off int) field {
		return field{tag: tag, typ: typeLong, data: binary.LittleEndian.AppendUint32(nil, uint32(off))}
	}

	var body bytes.Buffer
	writeIFD := func(fields []field) {
		binary.Write(&body, binary.LittleEndian, uint16(len(fields)))
		for _, f := range fields {
			encode(&body, f)
		}
		binary.Write(&body, binary.LittleEndian, uint32(0)) // no next IFD
	}
	writeIFD(append(append([]field{}, ifd0...),
		pointer(tagExifIFD, exifOff), pointer(tagGPSIFD, gpsOff)))
	writeIFD(exif)
	writeIFD(gps)

	var out bytes.Buffer
	out.Write([]byte{'I', 'I'})
	binary.Write(&out, binary.LittleEndian, uint16(42))
	binary.Write(&out, binary.LittleEndian, uint32(ifd0Off))
	out.Write(body.Bytes())
	out.Write(heap)
	return out.Bytes()
}

func TestParseEXIFOrientationAndCamera(t *testing.T) {
	exif := buildTIFF(
		[]field{
			short(tagOrientation, 6),
			ascii(tagMake, "Canon"),
			ascii(tagModel, "Canon EOS R6"),
		},
		[]field{ascii(tagDateTimeOrig, "2019:07:14 18:22:05")},
		nil,
	)
	data := jpegWithEXIF(t, 40, 30, exif)

	m, err := Analyze("image/jpeg", data)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if m.Width != 40 || m.Height != 30 {
		t.Errorf("dimensions = %dx%d, want 40x30", m.Width, m.Height)
	}
	if m.Orientation != 6 {
		t.Errorf("orientation = %d, want 6", m.Orientation)
	}
	// "Canon" + "Canon EOS R6" must not become "Canon Canon EOS R6".
	if m.Camera != "Canon EOS R6" {
		t.Errorf("camera = %q, want %q", m.Camera, "Canon EOS R6")
	}
	if m.TakenAt == nil {
		t.Fatal("taken_at not parsed")
	}
	if got := m.TakenAt.Format("2006-01-02T15:04:05"); got != "2019-07-14T18:22:05" {
		t.Errorf("taken_at = %s", got)
	}
}

func TestParseEXIFGPS(t *testing.T) {
	// 51°30'26.4"N, 0°7'39.36"W — a real coordinate, encoded the way a phone does.
	exif := buildTIFF(nil, nil, []field{
		ascii(gpsLatRef, "N"),
		rational(gpsLat, [2]uint32{51, 1}, [2]uint32{30, 1}, [2]uint32{264, 10}),
		ascii(gpsLonRef, "W"),
		rational(gpsLon, [2]uint32{0, 1}, [2]uint32{7, 1}, [2]uint32{3936, 100}),
	})

	m, err := Analyze("image/jpeg", jpegWithEXIF(t, 8, 8, exif))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if m.GPSLat == nil || m.GPSLon == nil {
		t.Fatalf("gps not parsed: %+v", m)
	}
	if *m.GPSLat < 51.5 || *m.GPSLat > 51.51 {
		t.Errorf("lat = %v, want ~51.507", *m.GPSLat)
	}
	// West is negative.
	if *m.GPSLon > -0.12 || *m.GPSLon < -0.13 {
		t.Errorf("lon = %v, want ~-0.1276", *m.GPSLon)
	}
}

// A camera that failed to get a fix writes exactly 0,0 far more often than
// anyone photographs that spot in the Atlantic. A pin there is worse than none.
func TestParseEXIFRejectsNullIsland(t *testing.T) {
	exif := buildTIFF(nil, nil, []field{
		ascii(gpsLatRef, "N"),
		rational(gpsLat, [2]uint32{0, 1}, [2]uint32{0, 1}, [2]uint32{0, 1}),
		ascii(gpsLonRef, "E"),
		rational(gpsLon, [2]uint32{0, 1}, [2]uint32{0, 1}, [2]uint32{0, 1}),
	})
	m, err := Analyze("image/jpeg", jpegWithEXIF(t, 8, 8, exif))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if m.GPSLat != nil || m.GPSLon != nil {
		t.Errorf("0,0 should be discarded, got lat=%v lon=%v", m.GPSLat, m.GPSLon)
	}
}

// A zero denominator is corrupt data, not zero degrees.
func TestParseEXIFRejectsZeroDenominator(t *testing.T) {
	exif := buildTIFF(nil, nil, []field{
		ascii(gpsLatRef, "N"),
		rational(gpsLat, [2]uint32{51, 0}, [2]uint32{30, 1}, [2]uint32{0, 1}),
		ascii(gpsLonRef, "E"),
		rational(gpsLon, [2]uint32{1, 1}, [2]uint32{0, 1}, [2]uint32{0, 1}),
	})
	m, err := Analyze("image/jpeg", jpegWithEXIF(t, 8, 8, exif))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if m.GPSLat != nil {
		t.Errorf("zero denominator should be rejected, got %v", *m.GPSLat)
	}
}

// A JPEG with no EXIF at all is completely normal and must still analyse.
func TestAnalyzeWithoutEXIF(t *testing.T) {
	m, err := Analyze("image/jpeg", jpegWithEXIF(t, 12, 8, nil))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if m.Width != 12 || m.Height != 8 {
		t.Errorf("dimensions = %dx%d, want 12x8", m.Width, m.Height)
	}
	// Orientation defaults to "as stored" so no client has to handle zero.
	if m.Orientation != 1 {
		t.Errorf("orientation = %d, want 1", m.Orientation)
	}
	if m.TakenAt != nil || m.Camera != "" || m.GPSLat != nil {
		t.Errorf("unexpected metadata from a bare JPEG: %+v", m)
	}
}

// Malformed EXIF must degrade to "no metadata", never panic and never fail the
// analysis — the dimensions are still perfectly usable.
func TestParseEXIFSurvivesHostileInput(t *testing.T) {
	cases := map[string][]byte{
		"truncated header": {'I', 'I', 42, 0},
		"bad magic":        {'I', 'I', 0xFF, 0xFF, 0, 0, 0, 8},
		"bad byte order":   {'X', 'Y', 42, 0, 8, 0, 0, 0},
		"ifd past end":     {'I', 'I', 42, 0, 0xF0, 0xFF, 0xFF, 0x7F},
		"huge entry count": {'I', 'I', 42, 0, 8, 0, 0, 0, 0xFF, 0xFF},
		"empty":            {},
		"offset overflow":  {'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		"self-referencing": selfReferencingIFD(),
		"nul-only ascii":   {'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x0F, 0x01, 2, 0, 4, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, exif := range cases {
		t.Run(name, func(t *testing.T) {
			data := jpegWithEXIF(t, 8, 8, exif)
			m, err := Analyze("image/jpeg", data)
			if err != nil {
				t.Fatalf("hostile EXIF must not fail the analysis: %v", err)
			}
			if m.Width != 8 || m.Height != 8 {
				t.Errorf("dimensions lost: %dx%d", m.Width, m.Height)
			}
		})
	}
}

// An IFD whose sub-IFD pointer points back at itself would loop forever without
// the depth bound.
func selfReferencingIFD() []byte {
	b := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	b = append(b, 1, 0) // one entry
	b = append(b, 0x69, 0x87, 4, 0, 1, 0, 0, 0, 8, 0, 0, 0)
	b = append(b, 0, 0, 0, 0)
	return b
}

func TestParseEXIFTimeFallsBackAndRejectsNonsense(t *testing.T) {
	if _, ok := parseEXIFTime("", ""); ok {
		t.Error("empty timestamp should not parse")
	}
	if _, ok := parseEXIFTime("not a date", ""); ok {
		t.Error("garbage should not parse")
	}
	// A camera whose clock was never set writes 1970 or earlier; a date there is
	// worse than none, because it anchors the whole timeline.
	if _, ok := parseEXIFTime("1899:01:01 00:00:00", ""); ok {
		t.Error("pre-1900 timestamp should be rejected")
	}
	// With an explicit offset the time is normalised to UTC.
	got, ok := parseEXIFTime("2020:03:01 12:00:00", "+05:30")
	if !ok {
		t.Fatal("offset form should parse")
	}
	if got.Format("15:04") != "06:30" {
		t.Errorf("offset not applied: %s", got.Format("15:04"))
	}
}

func TestToDegrees(t *testing.T) {
	if v, ok := toDegrees([]float64{51, 30, 26.4}, "N", "N", "S"); !ok || v < 51.5 || v > 51.51 {
		t.Errorf("north = %v ok=%v", v, ok)
	}
	if v, ok := toDegrees([]float64{51, 30, 26.4}, "S", "N", "S"); !ok || v > -51.5 {
		t.Errorf("south should be negative, got %v", v)
	}
	// An unrecognised hemisphere is unusable, not a guess.
	if _, ok := toDegrees([]float64{51, 0, 0}, "", "N", "S"); ok {
		t.Error("missing hemisphere should be rejected")
	}
	if _, ok := toDegrees([]float64{51, 0}, "N", "N", "S"); ok {
		t.Error("short triple should be rejected")
	}
}

func TestJoinCamera(t *testing.T) {
	cases := [][3]string{
		{"Canon", "Canon EOS R6", "Canon EOS R6"},
		{"NIKON CORPORATION", "NIKON D850", "NIKON CORPORATION NIKON D850"},
		{"Apple", "iPhone 15 Pro", "Apple iPhone 15 Pro"},
		{"", "Pixel 8", "Pixel 8"},
		{"Sony", "", "Sony"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := joinCamera(c[0], c[1]); got != c[2] {
			t.Errorf("joinCamera(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestIsMedia(t *testing.T) {
	for _, ct := range []string{"image/jpeg", "image/png", "image/gif", "video/mp4", "image/jpeg; charset=binary"} {
		if !IsMedia(ct) {
			t.Errorf("%q should be media", ct)
		}
	}
	// SVG is image/* but is not a raster image; it belongs to the text extractor.
	for _, ct := range []string{"image/svg+xml", "application/pdf", "text/plain", ""} {
		if IsMedia(ct) {
			t.Errorf("%q should not be media", ct)
		}
	}
}

func TestAnalyzeRejectsUndecodable(t *testing.T) {
	if _, err := Analyze("application/pdf", []byte("%PDF")); err != ErrUnsupported {
		t.Errorf("pdf: err = %v, want ErrUnsupported", err)
	}
	if _, err := Analyze("image/png", []byte("not a png")); err == nil {
		t.Error("undecodable image should error")
	}
}
