package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Everything the widened reader returns has to reach a column or the jsonb, or
// reading it off the file was pointless.
func TestApplyMetadataStoresEverythingTheFileCarried(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 0, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	altitude, direction, accuracy := 5.35, 138.08, 65.0
	fix := time.Date(2026, 4, 30, 22, 12, 0, 0, time.UTC)
	iso, flash, focal35, channels := 500, 24, 24, 2
	fnumber, exposure, focal, rate := 2.8, 1.0/60, 9.0, 29.97
	var bitrate int64 = 8543880
	captureType := 10

	err := s.ApplyMetadata(ctx, id, Metadata{
		Raw:          json.RawMessage(`{"ISO":500,"HDRHeadroom":0}`),
		GPSAltitude:  &altitude,
		GPSDirection: &direction,
		GPSAccuracy:  &accuracy,
		GPSAt:        &fix,

		ISO: &iso, FNumber: &fnumber, ExposureSeconds: &exposure,
		FocalLength: &focal, FocalLength35: &focal35, Flash: &flash,

		Description:  "a caption in the file",
		ColorProfile: "Display P3",
		CaptureType:  &captureType,

		VideoCodec: "hvc1", FrameRate: &rate, Bitrate: &bitrate,
		AudioCodec: "mp4a", AudioChannels: &channels,

		Faces: json.RawMessage(`[{"x":0.46,"y":0.79,"w":0.12,"h":0.16,"type":"Face"}]`),
	})
	if err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	var (
		gotAltitude, gotDirection, gotAccuracy float64
		gotAt                                  time.Time
		gotISO, gotFocal35, gotChannels        int
		gotFNumber, gotExposure, gotRate       float64
		gotBitrate                             int64
		gotProfile, gotCodec, gotCaption       string
		gotRaw, gotFaces                       []byte
		gotSubtypes                            []string
	)
	const query = `select gps_altitude, gps_direction, gps_accuracy, gps_at,
			iso, f_number, exposure_seconds, focal_length_35,
			color_profile, exif_description,
			video_codec, frame_rate, bitrate, audio_channels,
			exif_metadata, faces, subtypes
		from assets where id = $1::uuid`
	err = s.Pool().QueryRow(ctx, query, id).Scan(
		&gotAltitude, &gotDirection, &gotAccuracy, &gotAt,
		&gotISO, &gotFNumber, &gotExposure, &gotFocal35,
		&gotProfile, &gotCaption,
		&gotCodec, &gotRate, &gotBitrate, &gotChannels,
		&gotRaw, &gotFaces, &gotSubtypes)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if gotAltitude != altitude || gotDirection != direction || gotAccuracy != accuracy {
		t.Errorf("fix = %v/%v/%v, want %v/%v/%v",
			gotAltitude, gotDirection, gotAccuracy, altitude, direction, accuracy)
	}
	if !gotAt.Equal(fix) {
		t.Errorf("gps_at = %v, want %v", gotAt, fix)
	}
	if gotISO != iso || gotFNumber != fnumber || gotFocal35 != focal35 {
		t.Errorf("exposure = %d/%v/%d, want %d/%v/%d", gotISO, gotFNumber, gotFocal35, iso, fnumber, focal35)
	}
	if gotProfile != "Display P3" {
		t.Errorf("color_profile = %q", gotProfile)
	}
	if gotCodec != "hvc1" || gotChannels != channels || gotBitrate != bitrate {
		t.Errorf("stream = %q/%d/%d", gotCodec, gotChannels, gotBitrate)
	}
	if len(gotRaw) == 0 {
		t.Error("exif_metadata is empty; the verbatim answer was dropped")
	}
	if len(gotFaces) == 0 {
		t.Error("faces is empty")
	}
	if gotCaption != "a caption in the file" {
		t.Errorf("exif_description = %q", gotCaption)
	}
	if len(gotSubtypes) != 0 {
		t.Errorf("subtypes = %v, want none from a file read", gotSubtypes)
	}
}

// A caption typed into Google Photos outranks whatever a camera wrote into the
// file, and the metadata job runs again on every reindex — so this has to hold
// no matter which ran last.
func TestAFileCaptionFillsTheGapAndNeverOverwrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	described := seedAsset(t, s, 0, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, described, heicSidecar)
	if err := s.ApplyMetadata(ctx, described, Metadata{Description: "Screenshot"}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}

	got, err := s.Asset(ctx, described)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if got.Description != "at the border" {
		t.Errorf("Description = %q, want the sidecar's caption kept", got.Description)
	}

	// With no sidecar, the file's caption is the only one there is.
	bare := seedAsset(t, s, 1, time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))
	if err := s.ApplyMetadata(ctx, bare, Metadata{Description: "Screenshot"}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	got, err = s.Asset(ctx, bare)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if got.Description != "Screenshot" {
		t.Errorf("Description = %q, want the file's caption where nothing else has one", got.Description)
	}
}

const phoneSidecar = `{
  "favorite": true,
  "subtypes": ["livePhoto", "hdr"],
  "location": { "latitude": 41.7844, "longitude": -122.5848 }
}`

// The phone knows a heart, an album and "this is a screenshot", and nothing in
// the bytes it uploads records any of it.
func TestThePhonesOwnSidecarIsRecorded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	meta, err := ImportMetadataFrom(SourcePhotoKit, []byte(phoneSidecar),
		[]AlbumRef{{Title: "Iceland 2025"}})
	if err != nil {
		t.Fatalf("ImportMetadataFrom: %v", err)
	}
	if err := s.ApplyImportMetadata(ctx, id, meta); err != nil {
		t.Fatalf("ApplyImportMetadata: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if !got.Favorite {
		t.Error("Favorite = false; the heart did not survive")
	}
	if got.ImportSource != SourcePhotoKit {
		t.Errorf("ImportSource = %q, want %q", got.ImportSource, SourcePhotoKit)
	}

	var subtypes []string
	var lat *float64
	err = s.Pool().QueryRow(ctx,
		`select subtypes, import_gps_lat from assets where id = $1::uuid`, id).Scan(&subtypes, &lat)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subtypes) != 2 || subtypes[0] != "hdr" || subtypes[1] != "livePhoto" {
		t.Errorf("subtypes = %v, want both, sorted", subtypes)
	}
	// PhotoKit's coordinates go where a sidecar's go: they feed the canonical
	// pair only where the file itself carried nothing.
	if lat == nil {
		t.Error("import_gps_lat is null; the library's location was dropped")
	}

	extras, err := s.AssetExtras(ctx, id)
	if err != nil {
		t.Fatalf("AssetExtras: %v", err)
	}
	if len(extras.Albums) != 1 || extras.Albums[0] != "Iceland 2025" {
		t.Errorf("albums = %v", extras.Albums)
	}
}

// The same photograph can be described by the phone that took it and by an
// export of the service it was uploaded to. Neither erases the other.
func TestSubtypesMergeAcrossSources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 0, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	apply := func(sidecar string) {
		t.Helper()
		meta, err := ImportMetadataFrom(SourcePhotoKit, []byte(sidecar), nil)
		if err != nil {
			t.Fatalf("ImportMetadataFrom: %v", err)
		}
		if err := s.ApplyImportMetadata(ctx, id, meta); err != nil {
			t.Fatalf("ApplyImportMetadata: %v", err)
		}
	}
	apply(`{"subtypes": ["livePhoto"]}`)
	apply(`{"subtypes": ["hdr"]}`)
	// A source with nothing to say about subtypes is not asserting there are none.
	apply(`{"favorite": true}`)

	var subtypes []string
	if err := s.Pool().QueryRow(ctx,
		`select subtypes from assets where id = $1::uuid`, id).Scan(&subtypes); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(subtypes) != 2 {
		t.Errorf("subtypes = %v, want both kept", subtypes)
	}
}
