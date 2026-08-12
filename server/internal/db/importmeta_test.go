package db

import (
	"context"
	"testing"
	"time"
)

const heicSidecar = `{
  "title": "IMG_5874.HEIC",
  "description": "at the border",
  "photoTakenTime": { "timestamp": "1736125085" },
  "geoData": { "latitude": 41.7844, "longitude": -122.5848 },
  "favorited": true,
  "people": [{ "name": "Brody" }, { "name": "Dominic" }]
}`

func applySidecar(t *testing.T, s *Store, assetID, sidecar string, albums ...AlbumRef) {
	t.Helper()
	meta, err := ImportMetadataFrom(SourceGoogleTakeout, []byte(sidecar), albums)
	if err != nil {
		t.Fatalf("ImportMetadataFrom: %v", err)
	}
	if err := s.ApplyImportMetadata(context.Background(), assetID, meta); err != nil {
		t.Fatalf("ApplyImportMetadata: %v", err)
	}
}

func TestImportMetadataRecordsWhatTheSidecarKnows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	a.CapturedAt = nil
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Iceland 2025", Description: "the ring road"})

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.Description != "at the border" {
		t.Errorf("Description = %q", got.Description)
	}
	if !got.Favorite {
		t.Error("Favorite = false")
	}
	if got.ImportSource != SourceGoogleTakeout {
		t.Errorf("ImportSource = %q", got.ImportSource)
	}
	// The file itself carried no capture time, so the sidecar's is the only one
	// there is, and it has to reach the column the timeline sorts on.
	want := time.Unix(1736125085, 0).UTC()
	if got.CapturedAt == nil || !got.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, want)
	}
	if got.SortTime.Equal(got.UploadedAt) {
		t.Error("sort_time fell back to arrival despite the sidecar supplying a capture time")
	}

	extras, err := s.AssetExtras(ctx, id)
	if err != nil {
		t.Fatalf("AssetExtras: %v", err)
	}
	if len(extras.People) != 2 || extras.People[0] != "Brody" {
		t.Errorf("People = %v", extras.People)
	}
	if len(extras.Albums) != 1 || extras.Albums[0] != "Iceland 2025" {
		t.Errorf("Albums = %v", extras.Albums)
	}
}

// The whole sidecar is kept, not just the parts that became columns. The export
// is deleted the week after the import, and this is the only copy of the fields
// nobody has modelled yet.
func TestImportMetadataKeepsTheSidecarVerbatim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	applySidecar(t, s, id, `{"title": "x.HEIC", "imageViews": "6", "url": "https://photos.google.com/photo/AF1Q"}`)

	var url string
	err = s.pool.QueryRow(ctx,
		`select import_metadata->>'url' from assets where id = $1::uuid`, id).Scan(&url)
	if err != nil {
		t.Fatalf("read import_metadata: %v", err)
	}
	if url != "https://photos.google.com/photo/AF1Q" {
		t.Errorf("import_metadata->url = %q, want the field preserved", url)
	}
}

// The case that motivates keeping the sidecar's coordinates in their own
// columns. The metadata worker rewrites gps_lat on every run from what the file
// says, and for a screenshot the file says nothing — so a value merged straight
// into the column would survive only until the next reindex.
func TestSidecarCoordinatesSurviveTheMetadataJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	applySidecar(t, s, id, heicSidecar)

	// The file carries no coordinates of its own.
	if err := s.ApplyMetadata(ctx, id, Metadata{}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.GPSLat == nil || *got.GPSLat != 41.7844 {
		t.Fatalf("GPSLat = %v, want the sidecar's coordinates to survive", got.GPSLat)
	}

	// And the file wins wherever it has an opinion.
	lat, lon := 64.1466, -21.9426
	if err := s.ApplyMetadata(ctx, id, Metadata{GPSLat: &lat, GPSLon: &lon}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	if got, err = s.Asset(ctx, id); err != nil {
		t.Fatalf("reload asset: %v", err)
	}
	if got.GPSLat == nil || *got.GPSLat != lat {
		t.Errorf("GPSLat = %v, want the file's %v", got.GPSLat, lat)
	}
}

// The order of the two is not controlled: an import describes an asset the
// moment it lands, and the metadata job may have run first or not at all.
func TestSidecarCoordinatesArriveAfterTheMetadataJobToo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if err := s.ApplyMetadata(ctx, id, Metadata{}); err != nil {
		t.Fatalf("ApplyMetadata: %v", err)
	}
	applySidecar(t, s, id, heicSidecar)

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.GPSLat == nil || *got.GPSLat != 41.7844 {
		t.Errorf("GPSLat = %v, want the sidecar to fill the gap the job left", got.GPSLat)
	}
}

// The phone's capture time is better than Google's, and an import must not
// overwrite it.
func TestImportDoesNotOverwriteACaptureTimeAlreadyRecorded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	applySidecar(t, s, id, heicSidecar)

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(*a.CapturedAt) {
		t.Errorf("CapturedAt = %v, want the delivering client's %v", got.CapturedAt, a.CapturedAt)
	}
}

// A Takeout arrives as a stack of zips, an item can appear in a dated folder
// and an album folder both, and the way anyone recovers a half-finished import
// is to run it again. Applying twice must converge, and a second sidecar that
// happens to say less must not erase what the first one said.
func TestImportMetadataMergesRatherThanReplaces(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Iceland 2025"})
	applySidecar(t, s, id, `{"title": "IMG_5874.HEIC"}`, AlbumRef{Title: "Iceland 2025"}, AlbumRef{Title: "Best of"})

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if got.Description != "at the border" {
		t.Errorf("Description = %q, want the first sidecar's to survive a quieter second one", got.Description)
	}
	if !got.Favorite {
		t.Error("Favorite was cleared by a sidecar that did not mention it")
	}

	extras, err := s.AssetExtras(ctx, id)
	if err != nil {
		t.Fatalf("AssetExtras: %v", err)
	}
	if len(extras.Albums) != 2 {
		t.Errorf("Albums = %v, want both, each recorded once", extras.Albums)
	}
	if len(extras.People) != 2 {
		t.Errorf("People = %v, want no duplicates from the second run", extras.People)
	}
}

// Trash is recorded, not obeyed. The archive does not delete, and "Google had
// this in the bin" is a fact about Google.
func TestATrashedItemIsRecordedAsArchived(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	applySidecar(t, s, id, `{"title": "x.HEIC", "trashed": true}`)

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if !got.Archived {
		t.Error("Archived = false on a trashed item")
	}

	page, err := s.Timeline(ctx, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Error("an archived item vanished from the timeline; the flag is recorded, not acted on")
	}
}

// The Hidden album is recorded the same way Google's bin is: as a fact about
// where the source kept it, not as an instruction. An asset the phone hid is
// archived, flagged, and still in the timeline.
func TestAHiddenPhotoIsRecordedAsArchived(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	meta, err := ImportMetadataFrom(SourcePhotoKit, []byte(`{
		"favorite": false,
		"subtypes": [],
		"hidden": true,
		"photoKit": { "hidden": true, "hasAdjustments": true }
	}`), nil)
	if err != nil {
		t.Fatalf("ImportMetadataFrom: %v", err)
	}
	if err := s.ApplyImportMetadata(ctx, id, meta); err != nil {
		t.Fatalf("ApplyImportMetadata: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if !got.Archived {
		t.Error("Archived = false on a photo the phone had in the Hidden album")
	}

	page, err := s.Timeline(ctx, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 {
		t.Error("a hidden photo vanished from the timeline; the flag is recorded, not acted on")
	}
}

func TestAnUnknownImportSourceIsRefused(t *testing.T) {
	if _, err := ImportMetadataFrom("apple-photos", []byte(`{}`), nil); err == nil {
		t.Error("a format nothing can read was accepted")
	}
	if _, err := ImportMetadataFrom(SourceGoogleTakeout, nil, nil); err == nil {
		t.Error("an empty sidecar was accepted")
	}
}
