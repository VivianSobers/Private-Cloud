package media

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// The fixtures are built rather than checked in, for the reason mp4_test.go's
// are: a real recording is megabytes of frames wrapped around a few hundred
// bytes of header, and the header is the only part under test. Synthesising it
// keeps binary blobs out of the repository and puts every field in the
// assertion in plain sight of the test that asserts it.

// ebmlIDBytes writes an element id as the bytes it appears as on the wire. The
// length marker is part of the id, so this is just the significant bytes of the
// constant with its leading zeros dropped.
func ebmlIDBytes(id uint32) []byte {
	var full [4]byte
	binary.BigEndian.PutUint32(full[:], id)
	i := 0
	for i < 3 && full[i] == 0 {
		i++
	}
	return full[i:]
}

// ebmlSizeBytes encodes a length in the narrowest vint that can hold it, which
// is what a real muxer does — so the parser is exercised against 1-byte sizes
// for small elements and wider ones for the Segment, rather than against one
// convenient width everywhere.
func ebmlSizeBytes(n int64) []byte {
	for w := 1; w <= 8; w++ {
		// The all-ones value at each width is reserved for "unknown", so it is
		// the exclusive upper bound rather than the largest encodable length.
		limit := int64(1)<<uint(7*w) - 1
		if n < limit {
			b := make([]byte, w)
			v := n | int64(1)<<uint(7*w)
			for i := w - 1; i >= 0; i-- {
				b[i] = byte(v)
				v >>= 8
			}
			return b
		}
	}
	panic("ebml size out of range")
}

func elem(id uint32, body []byte) []byte {
	out := append([]byte{}, ebmlIDBytes(id)...)
	out = append(out, ebmlSizeBytes(int64(len(body)))...)
	return append(out, body...)
}

// elemUnknownSize writes an element whose length is the reserved all-ones
// value. Live-muxed WebM does this for the Segment, because its length is not
// known until the recording stops.
func elemUnknownSize(id uint32, body []byte) []byte {
	out := append([]byte{}, ebmlIDBytes(id)...)
	out = append(out, 0xFF) // one-byte vint, every data bit set
	return append(out, body...)
}

func ebmlUintBody(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return b[i:]
}

func ebmlFloat64Body(v float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
	return b
}

// A 32-bit float, because that is what most muxers write for a pose angle and
// it is the case where an exact 90 has to survive the round trip.
func ebmlFloat32Body(v float32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, math.Float32bits(v))
	return b
}

func ebmlHeader() []byte {
	// DocType and friends are not read by this parser; the header is present
	// because a real file has one and because the Segment must be found past it.
	return elem(idEBMLHeader, []byte{0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'})
}

type mkvOpts struct {
	timescale uint64  // TimestampScale, nanoseconds per tick
	duration  float64 // Duration, in ticks
	dateUTC   *time.Time
	width     uint64
	height    uint64
	display   [2]uint64 // DisplayWidth/DisplayHeight; zero means absent
	roll      *float32  // ProjectionPoseRoll in degrees; nil means no Projection
}

// sampleMKV builds a file with an audio track and a video track, in that order,
// so it is also the case that would break a parser taking the first track it
// finds.
func sampleMKV(o mkvOpts) []byte {
	var info []byte
	if o.timescale > 0 {
		info = append(info, elem(idTimestampScale, ebmlUintBody(o.timescale))...)
	}
	if o.duration > 0 {
		info = append(info, elem(idDuration, ebmlFloat64Body(o.duration))...)
	}
	if o.dateUTC != nil {
		ns := (o.dateUTC.Unix() - mkvEpochOffset) * 1e9
		info = append(info, elem(idDateUTC, ebmlUintBody(uint64(ns)))...)
	}

	var video []byte
	video = append(video, elem(idPixelWidth, ebmlUintBody(o.width))...)
	video = append(video, elem(idPixelHeight, ebmlUintBody(o.height))...)
	if o.display[0] > 0 {
		video = append(video, elem(idDisplayWidth, ebmlUintBody(o.display[0]))...)
		video = append(video, elem(idDisplayHeight, ebmlUintBody(o.display[1]))...)
	}
	if o.roll != nil {
		video = append(video, elem(idProjection, elem(idProjectionPoseRoll, ebmlFloat32Body(*o.roll)))...)
	}

	audioTrack := elem(idTrackEntry, elem(idTrackType, ebmlUintBody(2)))
	videoTrack := elem(idTrackEntry, concat(
		elem(idTrackType, ebmlUintBody(trackTypeVideo)),
		elem(idVideo, video),
	))
	tracks := elem(idTracks, concat(audioTrack, videoTrack))

	return concat(ebmlHeader(), elem(idSegment, concat(elem(idInfo, info), tracks)))
}

func TestAnalyzeMatroskaReadsTheEBMLTree(t *testing.T) {
	// 90 seconds at the default one-millisecond tick, 1920x1080, upright.
	data := sampleMKV(mkvOpts{timescale: 1_000_000, duration: 90_000, width: 1920, height: 1080})

	for _, mime := range []string{"video/x-matroska", "video/webm"} {
		t.Run(mime, func(t *testing.T) {
			m, err := Analyze(mime, data)
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
		})
	}
}

// Duration is a count of TimestampScale ticks, not milliseconds. A parser that
// ignores the scale is right only for the files that happen to use the default,
// which is most of them — which is exactly why it needs a test.
func TestMatroskaDurationScalesWithTheTimestampScale(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timescale uint64
		ticks     float64
		want      int64
	}{
		{"default millisecond tick", 1_000_000, 5000, 5000},
		{"microsecond tick", 1000, 5_000_000, 5000},
		{"ten-millisecond tick", 10_000_000, 500, 5000},
		{"absent, so the spec default applies", 0, 5000, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := sampleMKV(mkvOpts{timescale: tc.timescale, duration: tc.ticks, width: 640, height: 480})
			m, err := Analyze("video/webm", data)
			if err != nil {
				t.Fatal(err)
			}
			if m.DurationMS == nil || *m.DurationMS != tc.want {
				t.Errorf("duration = %v, want %dms", m.DurationMS, tc.want)
			}
		})
	}
}

// The same lie a phone tells in EXIF and in an MP4 display matrix, told a third
// way — and reported identically, so one viewer handles all three.
func TestMatroskaProjectionRollBecomesAnEXIFOrientation(t *testing.T) {
	for _, tc := range []struct {
		roll float32
		want int
	}{
		{0, 1},
		{90, 6},
		{180, 3},
		{270, 8},
		{-90, 8},  // the same turn as 270, written the other way round
		{-270, 6}, // and the same turn as 90
		{45, 1},   // not a quarter turn: EXIF cannot say it, so we do not guess
		{12.5, 1}, // nor this
		{360, 1},  // a full turn is no turn
	} {
		roll := tc.roll
		data := sampleMKV(mkvOpts{timescale: 1_000_000, width: 1920, height: 1080, roll: &roll})
		m, err := Analyze("video/x-matroska", data)
		if err != nil {
			t.Fatalf("roll %v: %v", tc.roll, err)
		}
		if m.Orientation != tc.want {
			t.Errorf("roll %v: orientation = %d, want %d", tc.roll, m.Orientation, tc.want)
		}
	}
}

// DisplayWidth/DisplayHeight are what a player lays out; the pixel dimensions
// are what was encoded. An anamorphic recording differs in the two, and the
// gallery needs the one it will actually draw.
func TestMatroskaPrefersDisplayDimensions(t *testing.T) {
	m, err := Analyze("video/webm", sampleMKV(mkvOpts{
		timescale: 1_000_000, width: 720, height: 576, display: [2]uint64{1024, 576},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 1024 || m.Height != 576 {
		t.Errorf("dimensions = %dx%d, want the 1024x576 a player would draw", m.Width, m.Height)
	}
}

// taken_at is the point of the media table. DateUTC counts nanoseconds from
// 2001, which is neither of the two epochs anything else here uses.
func TestMatroskaDateUTCBecomesTakenAt(t *testing.T) {
	want := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	m, err := Analyze("video/webm", sampleMKV(mkvOpts{
		timescale: 1_000_000, width: 640, height: 480, dateUTC: &want,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if m.TakenAt == nil {
		t.Fatal("taken_at is nil; DateUTC was not read")
	}
	if !m.TakenAt.Equal(want) {
		t.Errorf("taken_at = %v, want %v", m.TakenAt, want)
	}
}

func TestMatroskaWithoutDateUTCHasNoTakenAt(t *testing.T) {
	m, err := Analyze("video/webm", sampleMKV(mkvOpts{timescale: 1_000_000, width: 640, height: 480}))
	if err != nil {
		t.Fatal(err)
	}
	if m.TakenAt != nil {
		t.Errorf("taken_at = %v, want nil when the muxer wrote no DateUTC", m.TakenAt)
	}
}

// A live-muxed WebM does not know how long it will be, so its Segment declares
// an unknown length. Refusing to read those would exclude every screen
// recording and every stream capture.
func TestMatroskaSegmentWithUnknownSizeIsRead(t *testing.T) {
	info := elem(idInfo, concat(
		elem(idTimestampScale, ebmlUintBody(1_000_000)),
		elem(idDuration, ebmlFloat64Body(1234)),
	))
	tracks := elem(idTracks, elem(idTrackEntry, concat(
		elem(idTrackType, ebmlUintBody(trackTypeVideo)),
		elem(idVideo, concat(
			elem(idPixelWidth, ebmlUintBody(1280)),
			elem(idPixelHeight, ebmlUintBody(720)),
		)),
	)))
	data := concat(ebmlHeader(), elemUnknownSize(idSegment, concat(info, tracks)))

	m, err := Analyze("video/webm", data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 1280 || m.Height != 720 {
		t.Errorf("dimensions = %dx%d, want 1280x720", m.Width, m.Height)
	}
	if m.DurationMS == nil || *m.DurationMS != 1234 {
		t.Errorf("duration = %v, want 1234ms", m.DurationMS)
	}
}

// An unknown size anywhere else would make the parser walk frame data as if it
// were a tree. It stops instead, and the file degrades to the bare record.
func TestMatroskaUnknownSizeBelowTheSegmentStops(t *testing.T) {
	tracks := elemUnknownSize(idTracks, elem(idTrackEntry, elem(idVideo, concat(
		elem(idPixelWidth, ebmlUintBody(1280)),
		elem(idPixelHeight, ebmlUintBody(720)),
	))))
	data := concat(ebmlHeader(), elem(idSegment, tracks))

	m, err := Analyze("video/webm", data)
	if err != nil {
		t.Fatalf("should degrade, not error: %v", err)
	}
	if m.Width != 0 {
		t.Errorf("read dimensions past an unknown-size element: %+v", m)
	}
	if m.Source != "video" {
		t.Errorf("source = %q, want video", m.Source)
	}
}

// The dimensions belong to the video track, never to whichever track came
// first — and an audio track carrying a stray Video element must not win.
func TestMatroskaSkipsNonVideoTracks(t *testing.T) {
	audio := elem(idTrackEntry, concat(
		elem(idTrackType, ebmlUintBody(2)),
		elem(idVideo, concat(
			elem(idPixelWidth, ebmlUintBody(4096)),
			elem(idPixelHeight, ebmlUintBody(4096)),
		)),
	))
	video := elem(idTrackEntry, concat(
		elem(idTrackType, ebmlUintBody(trackTypeVideo)),
		elem(idVideo, concat(
			elem(idPixelWidth, ebmlUintBody(1280)),
			elem(idPixelHeight, ebmlUintBody(720)),
		)),
	))
	data := concat(ebmlHeader(), elem(idSegment, elem(idTracks, concat(audio, video))))

	m, err := Analyze("video/x-matroska", data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 1280 || m.Height != 720 {
		t.Errorf("dimensions = %dx%d, want 1280x720 — the audio track won", m.Width, m.Height)
	}
}

// A container is attacker-supplied input. None of these should panic, hang or
// read past the buffer; every one should degrade to the bare record.
func TestMalformedMatroskaDegradesRatherThanPanic(t *testing.T) {
	good := sampleMKV(mkvOpts{timescale: 1_000_000, duration: 1000, width: 640, height: 480})

	cases := map[string][]byte{
		"header only":                 ebmlHeader(),
		"truncated mid-element":       good[:len(good)-5],
		"truncated after the segment": concat(ebmlHeader(), ebmlIDBytes(idSegment)),
		"zero id byte":                concat(ebmlHeader(), []byte{0x00, 0x81, 0x00}),
		"zero size byte":              concat(ebmlHeader(), concat(ebmlIDBytes(idSegment), []byte{0x00})),
		"size past the buffer":        concat(ebmlHeader(), concat(ebmlIDBytes(idSegment), []byte{0x7F, 0xFF})),
		"empty segment":               concat(ebmlHeader(), elem(idSegment, nil)),
		"empty info":                  concat(ebmlHeader(), elem(idSegment, elem(idInfo, nil))),
		"track with an empty video":   concat(ebmlHeader(), elem(idSegment, elem(idTracks, elem(idTrackEntry, elem(idVideo, nil))))),
		"over-wide uint":              concat(ebmlHeader(), elem(idSegment, elem(idInfo, elem(idTimestampScale, make([]byte, 9))))),
		"float of a nonsense width":   concat(ebmlHeader(), elem(idSegment, elem(idInfo, elem(idDuration, make([]byte, 3))))),
		"absurd width":                sampleMKV(mkvOpts{timescale: 1_000_000, width: 1 << 30, height: 1 << 30}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := Analyze("video/x-matroska", data)
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
func TestDeeplyNestedEBMLIsBounded(t *testing.T) {
	inner := elem(idVideo, concat(
		elem(idPixelWidth, ebmlUintBody(640)),
		elem(idPixelHeight, ebmlUintBody(480)),
	))
	for i := 0; i < 200; i++ {
		inner = elem(idTrackEntry, inner)
	}
	data := concat(ebmlHeader(), elem(idSegment, elem(idTracks, inner)))

	if _, err := Analyze("video/webm", data); err != nil {
		t.Fatalf("deep nesting should be bounded, not an error: %v", err)
	}
}

// An MP4 is not a Matroska file and the reverse, and neither parser should
// claim the other's bytes.
func TestContainerDetectionDoesNotCross(t *testing.T) {
	if looksLikeMatroska(sampleMP4(t, 600, 600, 640, 480, 0, 0)) {
		t.Error("an MP4 was taken for Matroska")
	}
	if looksLikeMP4(sampleMKV(mkvOpts{timescale: 1_000_000, width: 640, height: 480})) {
		t.Error("a Matroska file was taken for an MP4")
	}
}
