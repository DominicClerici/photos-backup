package exifdata

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixtures are shared across the media packages, since they are binary and the
// same JPEG/HEIC/MOV serves all of them.
func fixture(name string) string { return filepath.Join("..", "..", "testdata", name) }

func TestReadPhotoWithFullEXIF(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("photo.jpg"))
	if err != nil {
		t.Fatalf("Read: %v\n\nIs exiftool installed? brew install exiftool", err)
	}

	if got.Width == nil || *got.Width != 800 {
		t.Errorf("Width = %v, want 800", got.Width)
	}
	if got.Height == nil || *got.Height != 600 {
		t.Errorf("Height = %v, want 600", got.Height)
	}
	if got.CameraMake != "Apple" {
		t.Errorf("CameraMake = %q, want Apple", got.CameraMake)
	}
	if got.CameraModel != "iPhone 14 Pro" {
		t.Errorf("CameraModel = %q", got.CameraModel)
	}
	if got.Lens == "" {
		t.Error("Lens is empty; the fixture carries LensModel")
	}

	// The fixture was written as 18:12 local at UTC-4, so the instant is 22:12Z.
	// Getting this wrong shifts photos across day boundaries in the timeline.
	want := time.Date(2026, 4, 30, 22, 12, 0, 0, time.UTC)
	if got.CapturedAt == nil {
		t.Fatal("CapturedAt is nil")
	}
	if !got.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt.UTC(), want)
	}
	if got.OffsetMinutes == nil || *got.OffsetMinutes != -240 {
		t.Errorf("OffsetMinutes = %v, want -240", got.OffsetMinutes)
	}

	if got.GPSLat == nil || *got.GPSLat < 44.47 || *got.GPSLat > 44.48 {
		t.Errorf("GPSLat = %v, want ~44.4759", got.GPSLat)
	}
	// West longitude must come back negative. exiftool only signs it when -n is
	// passed together with the reference tag, so this catches a dropped flag.
	if got.GPSLon == nil || *got.GPSLon > -73.2 || *got.GPSLon < -73.3 {
		t.Errorf("GPSLon = %v, want ~-73.2121", got.GPSLon)
	}
}

// A file with no metadata is normal, not an error: screenshots and
// messaging-app images arrive this way and still deserve a timeline slot.
func TestReadPhotoWithNoEXIF(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("bare.jpg"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.CapturedAt != nil {
		t.Errorf("CapturedAt = %v, want nil for a stripped file", got.CapturedAt)
	}
	if got.CameraMake != "" || got.CameraModel != "" {
		t.Errorf("camera = %q/%q, want empty", got.CameraMake, got.CameraModel)
	}
	// Dimensions come from the image itself, so they survive stripping.
	if got.Width == nil || *got.Width != 320 {
		t.Errorf("Width = %v, want 320", got.Width)
	}
}

func TestReadHEIC(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("sample.heic"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Width == nil || *got.Width != 400 {
		t.Errorf("Width = %v, want 400", got.Width)
	}
	if got.Height == nil || *got.Height != 300 {
		t.Errorf("Height = %v, want 300", got.Height)
	}
}

func TestReadVideoReportsDuration(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("clip.mov"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.DurationSeconds == nil {
		t.Fatal("DurationSeconds is nil for a video")
	}
	if *got.DurationSeconds < 0.5 || *got.DurationSeconds > 2 {
		t.Errorf("DurationSeconds = %v, want ~1", *got.DurationSeconds)
	}
	if got.Width == nil || *got.Width != 640 {
		t.Errorf("Width = %v, want 640", got.Width)
	}
}

// A real original off the phone, which is the only fixture that exercises the
// combination that actually ships: HEIC, a rotated sensor read, sub-second
// timestamps carrying their own offset, and GPS in degrees-minutes-seconds.
func TestReadRealIPhoneOriginal(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("iphone-portrait.heic"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Width == nil || *got.Width != 4032 || got.Height == nil || *got.Height != 3024 {
		t.Errorf("raw size = %v x %v, want the sensor's 4032x3024", got.Width, got.Height)
	}
	// The phone was upright, so what you should see is portrait.
	w, h := got.DisplaySize()
	if w == nil || *w != 3024 || h == nil || *h != 4032 {
		t.Errorf("DisplaySize = %v x %v, want 3024x4032", w, h)
	}

	// SubSecDateTimeOriginal carries "-04:00" inline, so this is exact.
	want := time.Date(2026, 8, 4, 23, 50, 36, 866_000_000, time.UTC)
	if got.CapturedAt == nil || !got.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, want)
	}
	if got.OffsetMinutes == nil || *got.OffsetMinutes != -240 {
		t.Errorf("OffsetMinutes = %v, want -240", got.OffsetMinutes)
	}

	if got.CameraModel != "iPhone 14 Pro" {
		t.Errorf("CameraModel = %q", got.CameraModel)
	}
	if got.GPSLat == nil || *got.GPSLat < 40.7 || *got.GPSLat > 40.73 {
		t.Errorf("GPSLat = %v, want ~40.721", got.GPSLat)
	}
	if got.GPSLon == nil || *got.GPSLon > -73.9 {
		t.Errorf("GPSLon = %v, want ~-73.978 (west is negative)", got.GPSLon)
	}
}

// The exposure, the altitude and the bearing are all in the same fixture the
// capture time comes from, and none of them used to be read.
func TestReadKeepsTheExposureAndTheRestOfTheFix(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("iphone-portrait.heic"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.ISO == nil || *got.ISO != 500 {
		t.Errorf("ISO = %v, want 500", got.ISO)
	}
	if got.FNumber == nil || *got.FNumber != 2.8 {
		t.Errorf("FNumber = %v, want 2.8", got.FNumber)
	}
	if got.ExposureSeconds == nil || *got.ExposureSeconds > 0.017 || *got.ExposureSeconds < 0.016 {
		t.Errorf("ExposureSeconds = %v, want ~1/60", got.ExposureSeconds)
	}
	if got.FocalLength == nil || *got.FocalLength != 9 {
		t.Errorf("FocalLength = %v, want 9", got.FocalLength)
	}
	if got.GPSAltitude == nil || *got.GPSAltitude < 5 || *got.GPSAltitude > 6 {
		t.Errorf("GPSAltitude = %v, want ~5.35", got.GPSAltitude)
	}
	if got.GPSDirection == nil || *got.GPSDirection < 138 || *got.GPSDirection > 139 {
		t.Errorf("GPSDirection = %v, want ~138", got.GPSDirection)
	}
	if got.ColorProfile != "Display P3" {
		t.Errorf("ColorProfile = %q, want Display P3", got.ColorProfile)
	}

	// Everything the file carries, kept as it was answered and qualified by the
	// group it came from, so a tag nobody thought to promote to a column is
	// still here to promote later.
	if len(got.Raw) == 0 {
		t.Fatal("Raw is empty; nothing was kept verbatim")
	}
	var raw map[string]any
	if err := json.Unmarshal(got.Raw, &raw); err != nil {
		t.Fatalf("Raw is not an object: %v", err)
	}
	for _, tag := range archivePaths {
		if _, ok := raw[tag]; ok {
			t.Errorf("Raw carries %s, which describes the blob's place on disk and not the photograph", tag)
		}
	}
	if _, ok := raw["EXIF:ISO"]; !ok {
		t.Error("Raw is missing EXIF:ISO, so it is not the whole answer")
	}
	// A tag this package never asks for by name. Before the wide pass it could
	// not be here at all, which is the whole point of the change: the choice of
	// what deserves a column is wrong about something, and this is what makes
	// being wrong recoverable without re-reading the archive.
	if _, ok := raw["EXIF:MeteringMode"]; !ok {
		t.Error("Raw is missing EXIF:MeteringMode, so it is still only the tags this package happens to name")
	}
	// Group-qualified, so two tags of the same name in different groups cannot
	// silently overwrite one another.
	for key := range raw {
		if !strings.Contains(key, ":") {
			t.Errorf("Raw key %q is not group-qualified", key)
			break
		}
	}
}

func TestReadDescribesAVideosStream(t *testing.T) {
	got, err := New().Read(context.Background(), fixture("live-clip.mov"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.VideoCodec != "avc1" {
		t.Errorf("VideoCodec = %q, want avc1", got.VideoCodec)
	}
	if got.FrameRate == nil || *got.FrameRate != 30 {
		t.Errorf("FrameRate = %v, want 30", got.FrameRate)
	}
	if got.AudioChannels == nil || *got.AudioChannels != 1 {
		t.Errorf("AudioChannels = %v, want 1", got.AudioChannels)
	}
	if got.Bitrate == nil || *got.Bitrate == 0 {
		t.Errorf("Bitrate = %v, want the container's average", got.Bitrate)
	}
}

// QuickTime writes CreateDate in UTC with no zone and CreationDate in local
// time with one. Reading only the first gives the right instant and loses the
// fact that it was mid-afternoon where the video was shot.
func TestCaptureTimePrefersQuickTimesLocalTime(t *testing.T) {
	got := raw{
		CreationDate:    "2019:08:12 12:36:15-07:00",
		CreateDate:      "2019:08:12 19:36:15",
		MediaCreateDate: "2019:08:12 19:36:15",
	}.toData()

	want := time.Date(2019, 8, 12, 19, 36, 15, 0, time.UTC)
	if got.CapturedAt == nil || !got.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, want)
	}
	if got.OffsetMinutes == nil || *got.OffsetMinutes != -420 {
		t.Errorf("OffsetMinutes = %v, want -420", got.OffsetMinutes)
	}
}

// EXIF wins over QuickTime where a file somehow has both: DateTimeOriginal is
// the photograph's own answer.
func TestCaptureTimeStillPrefersDateTimeOriginal(t *testing.T) {
	got := raw{
		DateTimeOriginal:   "2019:08:12 05:00:00",
		OffsetTimeOriginal: "+00:00",
		CreationDate:       "2019:08:12 12:36:15-07:00",
	}.toData()

	if got.CapturedAt == nil || got.CapturedAt.Hour() != 5 {
		t.Errorf("CapturedAt = %v, want the EXIF tag's 05:00", got.CapturedAt)
	}
}

// exiftool renders a one-element XMP list as a bare value and a longer one as
// an array, and writes the numbers as strings either way.
func TestFacesReadWhicheverShapeTheRegionsArriveIn(t *testing.T) {
	cases := map[string]struct {
		json string
		want int
	}{
		"one region, unwrapped": {`{
			"RegionType": "Face", "RegionAreaUnit": "normalized",
			"RegionAreaX": "0.46", "RegionAreaY": "0.79",
			"RegionAreaW": "0.12", "RegionAreaH": "0.16"
		}`, 1},
		"several regions": {`{
			"RegionType": ["Face", "Face"], "RegionAreaUnit": ["normalized", "normalized"],
			"RegionAreaX": ["0.47", "0.44"], "RegionAreaY": ["0.57", "0.45"],
			"RegionAreaW": ["0.04", "0.04"], "RegionAreaH": ["0.05", "0.05"]
		}`, 2},
		// Columns that do not line up cannot be zipped into boxes, and a box in
		// the wrong place is worse than none. The raw JSON still has them.
		"ragged arrays": {`{
			"RegionAreaX": ["0.47", "0.44"], "RegionAreaY": ["0.57"],
			"RegionAreaW": ["0.04", "0.04"], "RegionAreaH": ["0.05", "0.05"]
		}`, 0},
		// A region value that is not a number costs the boxes and nothing else.
		// Refusing the record would fail the metadata job over an XMP oddity and
		// take the capture time and the exposure down with it.
		"a value that is not a number": {`{
			"RegionAreaX": "middle", "RegionAreaY": "0.57",
			"RegionAreaW": "0.04", "RegionAreaH": "0.05"
		}`, 0},
		// A region measured in pixels means something else entirely.
		"pixel units": {`{
			"RegionAreaUnit": "pixel",
			"RegionAreaX": "1681", "RegionAreaY": "1618",
			"RegionAreaW": "751", "RegionAreaH": "753"
		}`, 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var x raw
			if err := json.Unmarshal([]byte(tc.json), &x); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := x.toData().Faces
			if len(got) != tc.want {
				t.Fatalf("faces = %v, want %d", got, tc.want)
			}
			for _, face := range got {
				if face.X <= 0 || face.X >= 1 || face.W <= 0 || face.W >= 1 {
					t.Errorf("face %v is not a fraction of the image", face)
				}
			}
		})
	}
}

// A 2008 JPEG in the real library records "ISO": 75.4582213796711, and a 2019
// HEIC records "Software": 12.4 where a string belongs. Decoded strictly, one
// odd tag is not an error in that tag — it is an error for the whole record,
// which fails the metadata job, retries four more times, and finally marks a
// perfectly good photograph broken and thumbnail-less over a number nothing was
// ever going to display.
func TestAnOddlyTypedTagCostsThatTagAndNothingElse(t *testing.T) {
	var x raw
	err := json.Unmarshal([]byte(`{
		"ISO": 75.4582213796711,
		"Model": 12.4,
		"FocalLengthIn35mmFormat": "26",
		"AudioChannels": "undef",
		"GPSAltitude": "inf",
		"DateTimeOriginal": "2008:06:14 11:02:31"
	}`), &x)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := x.toData()
	if got.ISO == nil || *got.ISO != 75 {
		t.Errorf("ISO = %v, want the fraction rounded to 75", got.ISO)
	}
	if got.CameraModel != "12.4" {
		t.Errorf("CameraModel = %q, want the number rendered as text", got.CameraModel)
	}
	if got.FocalLength35 == nil || *got.FocalLength35 != 26 {
		t.Errorf("FocalLength35 = %v, want 26 from the string", got.FocalLength35)
	}
	if got.AudioChannels != nil {
		t.Errorf("AudioChannels = %v, want nil for a tag that is not a number", got.AudioChannels)
	}
	if got.GPSAltitude != nil {
		t.Errorf("GPSAltitude = %v, want nil — an infinity is not a measurement", got.GPSAltitude)
	}
	// And the rest of the record survived, which is the whole point.
	if got.CapturedAt == nil {
		t.Error("CapturedAt is nil; one odd tag took the capture time with it")
	}
}

// A screenshot's UserComment is a bare newline, which is not a caption.
func TestDescriptionTakesTheFirstTagThatSaysSomething(t *testing.T) {
	got := raw{UserComment: "\n", CaptionAbstract: " a caption "}.toData()
	if got.Description != "a caption" {
		t.Errorf("Description = %q, want the trimmed caption", got.Description)
	}

	if empty := (raw{UserComment: "  "}).toData(); empty.Description != "" {
		t.Errorf("Description = %q, want empty for whitespace", empty.Description)
	}
}

func TestDisplaySizeLeavesUprightImagesAlone(t *testing.T) {
	w, h := 800, 600
	for _, orientation := range []int{1, 2, 3, 4} {
		d := Data{Width: &w, Height: &h, Orientation: &orientation}
		gotW, gotH := d.DisplaySize()
		if *gotW != 800 || *gotH != 600 {
			t.Errorf("orientation %d: DisplaySize = %dx%d, want 800x600", orientation, *gotW, *gotH)
		}
	}
}

func TestDisplaySizeToleratesAMissingOrientation(t *testing.T) {
	w, h := 800, 600
	gotW, gotH := Data{Width: &w, Height: &h}.DisplaySize()
	if *gotW != 800 || *gotH != 600 {
		t.Errorf("DisplaySize = %dx%d, want 800x600", *gotW, *gotH)
	}

	gotW, gotH = Data{}.DisplaySize()
	if gotW != nil || gotH != nil {
		t.Error("DisplaySize invented dimensions for a file that reported none")
	}
}

func TestReadRejectsAFileExiftoolCannotOpen(t *testing.T) {
	_, err := New().Read(context.Background(), fixture("does-not-exist.jpg"))
	if err == nil {
		t.Fatal("Read on a missing file returned no error")
	}
}

func TestParseCapturePrefersTheZoneOnTheTimestamp(t *testing.T) {
	// A zone appended to the value describes that tag exactly, so it must win
	// over OffsetTimeOriginal, which may have been copied from another tag.
	at, offset, ok := parseCapture("2026:04:30 18:12:00+02:00", "-04:00")
	if !ok {
		t.Fatal("parseCapture returned not-ok for a zoned timestamp")
	}
	want := time.Date(2026, 4, 30, 16, 12, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Errorf("at = %v, want %v", at, want)
	}
	if offset == nil || *offset != 120 {
		t.Errorf("offset = %v, want 120", offset)
	}
}

func TestParseCaptureFallsBackToUTCWithNoOffsetReported(t *testing.T) {
	at, offset, ok := parseCapture("2026:04:30 18:12:00")
	if !ok {
		t.Fatal("parseCapture returned not-ok for a bare timestamp")
	}
	if !at.Equal(time.Date(2026, 4, 30, 18, 12, 0, 0, time.UTC)) {
		t.Errorf("at = %v", at)
	}
	// nil is the signal that the zone was assumed, not read.
	if offset != nil {
		t.Errorf("offset = %v, want nil when the file recorded no zone", *offset)
	}
}

func TestParseCaptureRejectsAnUnsetDate(t *testing.T) {
	for _, value := range []string{"", "   ", "0000:00:00 00:00:00", "not a date"} {
		if _, _, ok := parseCapture(value); ok {
			t.Errorf("parseCapture(%q) accepted an unusable value", value)
		}
	}
}

func TestParseCaptureHandlesSubSecondsAndHalfHourZones(t *testing.T) {
	at, offset, ok := parseCapture("2026:04:30 18:12:00.482", "+05:30")
	if !ok {
		t.Fatal("parseCapture returned not-ok")
	}
	want := time.Date(2026, 4, 30, 12, 42, 0, 482_000_000, time.UTC)
	if !at.Equal(want) {
		t.Errorf("at = %v, want %v", at, want)
	}
	if offset == nil || *offset != 330 {
		t.Errorf("offset = %v, want 330", offset)
	}
}

func TestParseOffsetAcceptsZulu(t *testing.T) {
	got, ok := parseOffset("Z")
	if !ok || got != 0 {
		t.Errorf("parseOffset(Z) = %d, %v; want 0, true", got, ok)
	}
	if _, ok := parseOffset("nonsense"); ok {
		t.Error("parseOffset accepted nonsense")
	}
}
