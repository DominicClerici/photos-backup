package exifdata

import (
	"context"
	"path/filepath"
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
