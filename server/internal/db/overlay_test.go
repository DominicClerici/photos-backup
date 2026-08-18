package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// snapchatSidecar builds the document the Snapchat importer sends. Composed
// rather than copied from an export, because Snapchat writes one document for
// the whole account and this archive stores one per file — the row under
// "history" is Snapchat's, verbatim, and everything around it is the
// importer's reading.
func snapchatSidecar(t *testing.T, role string, subtypes []string, history string) []byte {
	t.Helper()
	doc := map[string]any{
		"export":           "snapchat",
		"kind":             "memory",
		"role":             role,
		"capturedAt":       "2017-09-02T06:55:44Z",
		"capturedAtSource": "history",
		"subtypes":         subtypes,
	}
	if history != "" {
		doc["history"] = json.RawMessage(history)
		doc["historyMatch"] = "exact"
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	return raw
}

func recordOverlayAsset(t *testing.T, s *Store, sha string) string {
	t.Helper()
	a := sampleAsset()
	a.SHA256 = sha
	a.MD5 = "0f343b0931126a20f133d67c2b018a3b"
	a.OriginalFilename = "2017-09-02_abc-overlay.png"
	a.Ext = ".png"
	a.ContentType = "image/png"
	a.LocalID = "memories/2017-09-02_abc-overlay.png"

	id, _, err := s.RecordAsset(context.Background(), a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	return id
}

const overlaySHA = "1111111111111111111111111111111111111111111111111111111111111111"

// The Snapchat sidecar reaches the columns the gallery reads. It matters more
// than the Takeout equivalent because Snapchat strips EXIF from every still:
// there is no second source for a memory's time or place.
func TestImportMetadataFromSnapchat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	a.CapturedAt = nil
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	sidecar := snapchatSidecar(t, "main", []string{"snapchat:memory"},
		`{"Date":"2017-09-02 06:55:44 UTC","Media Type":"Image",`+
			`"Location":"Latitude, Longitude: 39.161533, -86.532104"}`)
	meta, err := ImportMetadataFrom(SourceSnapchat, sidecar, nil)
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
	if got.ImportSource != SourceSnapchat {
		t.Errorf("ImportSource = %q, want %q", got.ImportSource, SourceSnapchat)
	}
	want := time.Date(2017, 9, 2, 6, 55, 44, 0, time.UTC)
	if got.CapturedAt == nil || !got.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, want)
	}
	if got.Archived {
		t.Error("a memory was archived; only overlays should be")
	}
}

// An unknown source is still refused. Adding a third reader must not have
// turned the check into a formality.
func TestImportMetadataRefusesUnknownSource(t *testing.T) {
	if _, err := ImportMetadataFrom("instagram", []byte(`{}`), nil); err == nil {
		t.Fatal("an unknown import source was accepted")
	}
}

// The link is by content hash, and it writes both ends: the photo learns which
// blob its handwriting is in, and the overlay learns not to appear in the
// gallery as a photograph of its own.
func TestLinkOverlaySetsBothEnds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mainID, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	overlayID := recordOverlayAsset(t, s, overlaySHA)

	if err := s.LinkOverlay(ctx, mainID, overlaySHA); err != nil {
		t.Fatalf("LinkOverlay: %v", err)
	}

	var linked *string
	var mainIsOverlay, overlayIsOverlay bool
	err = s.pool.QueryRow(ctx,
		`select overlay_asset_id::text, is_overlay from assets where id = $1::uuid`,
		mainID).Scan(&linked, &mainIsOverlay)
	if err != nil {
		t.Fatalf("read the memory: %v", err)
	}
	if linked == nil || *linked != overlayID {
		t.Errorf("overlay_asset_id = %v, want %s", linked, overlayID)
	}
	if mainIsOverlay {
		t.Error("the photograph was marked as an overlay")
	}

	if err := s.pool.QueryRow(ctx,
		`select is_overlay from assets where id = $1::uuid`, overlayID).Scan(&overlayIsOverlay); err != nil {
		t.Fatalf("read the overlay: %v", err)
	}
	if !overlayIsOverlay {
		t.Error("the overlay was not marked as one, so the timeline will draw it")
	}
}

// Re-running an import is how anyone recovers a half-finished one, so linking
// twice has to be the same as linking once.
func TestLinkOverlayIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mainID, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	recordOverlayAsset(t, s, overlaySHA)

	for range 2 {
		if err := s.LinkOverlay(ctx, mainID, overlaySHA); err != nil {
			t.Fatalf("LinkOverlay: %v", err)
		}
	}
}

// A hash for a blob the archive does not hold is an importer that named an
// overlay it never uploaded. Saying so beats recording a link to nothing.
func TestLinkOverlayRejectsUnknownHash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mainID, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	err = s.LinkOverlay(ctx, mainID,
		"2222222222222222222222222222222222222222222222222222222222222222")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LinkOverlay returned %v, want ErrNotFound", err)
	}
}

// Linking an asset to itself would hide a photograph from the timeline forever,
// and no filename in any export could produce it honestly.
func TestLinkOverlayRefusesSelfReference(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	id, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	if err := s.LinkOverlay(ctx, id, a.SHA256); err == nil {
		t.Fatal("an asset was allowed to be its own overlay")
	}
}

// An overlay carries the archived flag from its subtype and is kept out of the
// timeline by is_overlay. The two are separate on purpose: archived records
// what the source thought, is_overlay records that this is part of another
// picture rather than a picture.
func TestSnapchatOverlayIsArchivedAndHidden(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mainID, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	overlayID := recordOverlayAsset(t, s, overlaySHA)

	meta, err := ImportMetadataFrom(SourceSnapchat,
		snapchatSidecar(t, "overlay", []string{"snapchat:memory", "snapchat:overlay"}, ""), nil)
	if err != nil {
		t.Fatalf("ImportMetadataFrom: %v", err)
	}
	if err := s.ApplyImportMetadata(ctx, overlayID, meta); err != nil {
		t.Fatalf("ApplyImportMetadata: %v", err)
	}
	if err := s.LinkOverlay(ctx, mainID, overlaySHA); err != nil {
		t.Fatalf("LinkOverlay: %v", err)
	}

	got, err := s.Asset(ctx, overlayID)
	if err != nil {
		t.Fatalf("load overlay: %v", err)
	}
	if !got.Archived {
		t.Error("the overlay was not archived")
	}

	// The timeline must show the photograph and not the handwriting.
	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 50)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	for _, item := range page.Items {
		if item.ID == overlayID {
			t.Fatal("the overlay appears in the timeline")
		}
	}
	found := false
	for _, item := range page.Items {
		if item.ID == mainID {
			found = true
		}
	}
	if !found {
		t.Error("the photograph the overlay belongs to is missing from the timeline")
	}
}
