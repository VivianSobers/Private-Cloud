package media

import (
	"encoding/binary"
	"testing"
	"time"
)

// The fixtures are built rather than checked in. A real recording is megabytes
// of media data wrapped around a few hundred bytes of header, and the header is
// the only part under test — synthesising it keeps the repository free of binary
// blobs and, more usefully, makes every field in the assertion visible in the
// test that asserts it.

func box(typ string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], typ)
	return append(out, body...)
}

// rotationMatrix builds the 3x3 display matrix for a quarter turn, in the 16.16
// fixed point the container stores.
func rotationMatrix(degrees int) []byte {
	const one = 1 << 16
	var a, b, c, d int32
	switch degrees {
	case 90:
		a, b, c, d = 0, one, -one, 0
	case 180:
		a, b, c, d = -one, 0, 0, -one
	case 270:
		a, b, c, d = 0, -one, one, 0
	default:
		a, b, c, d = one, 0, 0, one
	}
	m := make([]byte, 36)
	binary.BigEndian.PutUint32(m[0:], uint32(a))
	binary.BigEndian.PutUint32(m[4:], uint32(b))
	binary.BigEndian.PutUint32(m[12:], uint32(c))
	binary.BigEndian.PutUint32(m[16:], uint32(d))
	binary.BigEndian.PutUint32(m[32:], 1<<30) // w, conventionally 1.0 in 2.30
	return m
}

func mvhdV0(timescale, duration uint32, created uint32) []byte {
	b := make([]byte, 100)
	b[0] = 0 // version 0
	binary.BigEndian.PutUint32(b[4:], created)
	binary.BigEndian.PutUint32(b[8:], created)
	binary.BigEndian.PutUint32(b[12:], timescale)
	binary.BigEndian.PutUint32(b[16:], duration)
	return box("mvhd", b)
}

func tkhdV0(width, height uint32, degrees int) []byte {
	// version/flags(4) created(4) modified(4) id(4) reserved(4) duration(4)
	// reserved(8) layer(2) alt(2) volume(2) reserved(2) matrix(36) w(4) h(4)
	b := make([]byte, 4+4+4+4+4+4+16+36+8)
	b[0] = 0
	matrix := 4 + 4 + 4 + 4 + 4 + 4 + 16
	copy(b[matrix:], rotationMatrix(degrees))
	binary.BigEndian.PutUint32(b[matrix+36:], width<<16)
	binary.BigEndian.PutUint32(b[matrix+40:], height<<16)
	return box("tkhd", b)
}

func ftyp() []byte { return box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2")) }

// A recording carries a video track and an audio track. The audio track's tkhd
// is a legitimate 0x0, so this is also the case that would break a parser that
// simply took the first track it found.
func sampleMP4(t *testing.T, timescale, duration uint32, w, h uint32, degrees int, created uint32) []byte {
	t.Helper()
	video := box("trak", tkhdV0(w, h, degrees))
	audio := box("trak", tkhdV0(0, 0, 0))
	moov := box("moov", concat(mvhdV0(timescale, duration, created), audio, video))
	return concat(ftyp(), moov)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestAnalyzeVideoReadsTheContainerHeader(t *testing.T) {
	// 90 seconds at a 600 timescale, 1920x1080, upright.
	data := sampleMP4(t, 600, 600*90, 1920, 1080, 0, 0)

	m, err := Analyze("video/mp4", data)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if m.Source != "video" {
		t.Errorf("source = %q, want video", m.Source)
	}
	if m.Width != 1920 || m.Height != 1080 {
		t.Errorf("dimensions = %dx%d, want 1920x1080", m.Width, m.Height)
	}
	if m.DurationMS == nil || *m.DurationMS != 90_000 {
		t.Errorf("duration = %v, want 90000ms", m.DurationMS)
	}
	if m.Orientation != 1 {
		t.Errorf("orientation = %d, want 1", m.Orientation)
	}
}

// A phone recorded in portrait writes landscape dimensions plus a rotation, the
// same shape of lie EXIF orientation tells about photos — so it is reported the
// same way and one viewer handles both.
func TestVideoRotationBecomesAnEXIFOrientation(t *testing.T) {
	for _, tc := range []struct {
		degrees int
		want    int
	}{
		{0, 1}, {90, 6}, {180, 3}, {270, 8},
	} {
		data := sampleMP4(t, 1000, 5000, 1920, 1080, tc.degrees, 0)
		m, err := Analyze("video/mp4", data)
		if err != nil {
			t.Fatalf("%d degrees: %v", tc.degrees, err)
		}
		if m.Orientation != tc.want {
			t.Errorf("%d degrees: orientation = %d, want %d", tc.degrees, m.Orientation, tc.want)
		}
	}
}

// The dimensions belong to the video track, never to whichever track came first.
func TestVideoDimensionsComeFromTheLargestTrack(t *testing.T) {
	m, err := Analyze("video/mp4", sampleMP4(t, 600, 600, 1280, 720, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 1280 || m.Height != 720 {
		t.Errorf("dimensions = %dx%d, want 1280x720 — the 0x0 audio track won", m.Width, m.Height)
	}
}

// taken_at is the point of the media table, and a container that records when
// the shutter opened should populate it rather than leaving the timeline to sort
// by upload.
func TestVideoCreationTimeBecomesTakenAt(t *testing.T) {
	want := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	data := sampleMP4(t, 600, 600, 640, 480, 0, uint32(want.Unix()+mp4EpochOffset))

	m, err := Analyze("video/mp4", data)
	if err != nil {
		t.Fatal(err)
	}
	if m.TakenAt == nil {
		t.Fatal("taken_at is nil; the creation time was not read")
	}
	if !m.TakenAt.Equal(want) {
		t.Errorf("taken_at = %v, want %v", m.TakenAt, want)
	}
}

// Zero means "the muxer did not set this", not 1904.
func TestVideoUnsetCreationTimeStaysAbsent(t *testing.T) {
	m, err := Analyze("video/mp4", sampleMP4(t, 600, 600, 640, 480, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if m.TakenAt != nil {
		t.Errorf("taken_at = %v, want nil for an unset creation time", m.TakenAt)
	}
}

// The case the prefix bound makes ordinary: a recording whose header was written
// after the media data, of which we were handed only the beginning. It must
// still be a video in the timeline.
func TestVideoWithoutReachableHeaderStillCountsAsMedia(t *testing.T) {
	mdat := box("mdat", make([]byte, 4096))
	data := concat(ftyp(), mdat) // moov would follow, beyond the prefix

	m, err := Analyze("video/mp4", data)
	if err != nil {
		t.Fatalf("a header we cannot reach is not an error: %v", err)
	}
	if m.Source != "video" {
		t.Errorf("source = %q, want video", m.Source)
	}
	if m.Width != 0 || m.DurationMS != nil {
		t.Errorf("invented metadata from a header it never saw: %+v", m)
	}
	if m.Orientation != 1 {
		t.Errorf("orientation = %d, want 1", m.Orientation)
	}
}

// A container is attacker-supplied input. None of these should panic, hang, or
// read past the buffer; every one should degrade to the bare record.
func TestMalformedContainersDegradeRatherThanPanic(t *testing.T) {
	cases := map[string][]byte{
		"truncated after the type": []byte("\x00\x00\x00\x20ftyp"),
		"box larger than the file": concat(ftyp(), []byte("\x7f\xff\xff\xffmoov")),
		"zero-length box":          concat(ftyp(), []byte("\x00\x00\x00\x00moov")),
		"size below the header":    concat(ftyp(), []byte("\x00\x00\x00\x01moov")),
		"moov with a short mvhd":   concat(ftyp(), box("moov", box("mvhd", []byte{0, 0}))),
		"moov with a short tkhd":   concat(ftyp(), box("moov", box("trak", box("tkhd", []byte{0})))),
		"tkhd with a bad version":  concat(ftyp(), box("moov", box("trak", box("tkhd", []byte{9, 0, 0, 0})))),
		"empty moov":               concat(ftyp(), box("moov", nil)),
		"only a header":            []byte("\x00\x00\x00\x08ftyp"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := Analyze("video/mp4", data)
			if err != nil {
				t.Fatalf("malformed input should degrade, not error: %v", err)
			}
			if m.Source != "video" {
				t.Errorf("source = %q, want video", m.Source)
			}
			if m.Orientation < 1 || m.Orientation > 8 {
				t.Errorf("orientation = %d, outside the 1-8 the column allows", m.Orientation)
			}
		})
	}
}

// Deep nesting must not recurse without bound.
func TestDeeplyNestedBoxesAreBounded(t *testing.T) {
	inner := box("tkhd", make([]byte, 100))
	for i := 0; i < 200; i++ {
		inner = box("trak", inner)
	}
	data := concat(ftyp(), box("moov", inner))

	if _, err := Analyze("video/mp4", data); err != nil {
		t.Fatalf("deep nesting should be bounded, not an error: %v", err)
	}
}

// A zero timescale is a divide by zero waiting to happen, and there is no
// sensible unit to fall back to.
func TestZeroTimescaleYieldsNoDuration(t *testing.T) {
	data := concat(ftyp(), box("moov", concat(mvhdV0(0, 1000, 0), box("trak", tkhdV0(640, 480, 0)))))
	m, err := Analyze("video/mp4", data)
	if err != nil {
		t.Fatal(err)
	}
	if m.DurationMS != nil {
		t.Errorf("duration = %v, want nil when the timescale is zero", m.DurationMS)
	}
	if m.Width != 640 {
		t.Errorf("a bad mvhd should not cost the dimensions: %+v", m)
	}
}

// 0xFFFFFFFF is the convention for a file still being written.
func TestUnknownDurationStaysAbsent(t *testing.T) {
	data := concat(ftyp(), box("moov", mvhdV0(600, 0xFFFFFFFF, 0)))
	m, err := Analyze("video/mp4", data)
	if err != nil {
		t.Fatal(err)
	}
	if m.DurationMS != nil {
		t.Errorf("duration = %v, want nil for the unknown-duration sentinel", m.DurationMS)
	}
}
