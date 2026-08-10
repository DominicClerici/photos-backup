package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func sampleAsset() Asset {
	captured := time.Date(2026, 8, 1, 15, 4, 5, 0, time.UTC)
	return Asset{
		SHA256:           "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		MD5:              "d41d8cd98f00b204e9800998ecf8427e",
		ByteSize:         2_411_255,
		OriginalFilename: "IMG_8071.HEIC",
		Ext:              ".heic",
		ContentType:      "image/heic",
		CapturedAt:       &captured,
		DeviceID:         "iphone-14-pro",
		LocalID:          "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001",
	}
}

func TestRecordAssetReturnsNewID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, inserted, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	if !inserted {
		t.Errorf("inserted = false, want true for a new sha256")
	}
	if id == "" {
		t.Error("RecordAsset returned an empty id")
	}
}

func TestRecordAssetIsIdempotentOnSHA256(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	firstID, _, err := s.RecordAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}

	// Same bytes arriving again from a different local asset must not create a
	// second row; the archive is addressed by content, not by phone identity.
	dup := sampleAsset()
	dup.LocalID = "a-different-local-id"
	secondID, inserted, err := s.RecordAsset(ctx, dup)
	if err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}
	if inserted {
		t.Errorf("inserted = true on duplicate sha256, want false")
	}
	if secondID != firstID {
		t.Errorf("duplicate returned id %q, want the original %q", secondID, firstID)
	}

	assets, err := s.RecentAssets(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAssets: %v", err)
	}
	if len(assets) != 1 {
		t.Errorf("table holds %d rows after duplicate upload, want 1", len(assets))
	}
}

// The mapping has to survive content dedup. If it does not, sync/check asks for
// the second local id on every run and the phone re-uploads it forever.
func TestRecordAssetMapsBothLocalIDsOfDuplicateContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, first); err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	second := sampleAsset()
	second.LocalID = "a-different-local-id"
	assetID, _, err := s.RecordAsset(ctx, second)
	if err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	known, err := s.KnownMappings(ctx, first.DeviceID, []LocalRef{
		{LocalID: first.LocalID},
		{LocalID: second.LocalID},
	})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 2 {
		t.Fatalf("KnownMappings returned %d mappings, want 2: %v", len(known), known)
	}
	if known[first.LocalID] != assetID || known[second.LocalID] != assetID {
		t.Errorf("both local ids should resolve to %q, got %v", assetID, known)
	}
}

func TestKnownMappingsSkipsOtherDevices(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	known, err := s.KnownMappings(ctx, "some-other-phone", []LocalRef{{LocalID: a.LocalID}})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 0 {
		t.Errorf("another device's local id resolved: %v", known)
	}
}

// A photo edited in Photos keeps its local identifier but changes its bytes, so
// a stale modification time must not be reported as already archived.
func TestKnownMappingsRejectsChangedModificationTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	modified := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	a := sampleAsset()
	a.ModifiedAt = &modified
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	same, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID, ModifiedAt: &modified}})
	if err != nil {
		t.Fatalf("KnownMappings unchanged: %v", err)
	}
	if len(same) != 1 {
		t.Errorf("unchanged asset did not resolve: %v", same)
	}

	edited := modified.Add(time.Hour)
	after, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID, ModifiedAt: &edited}})
	if err != nil {
		t.Fatalf("KnownMappings edited: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("edited asset resolved as already archived: %v", after)
	}
}

// Rows written before modification times were tracked hold null, and a phone
// that reports no modification time must still match them.
func TestKnownMappingsMatchesTwoNullModificationTimes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	known, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID}})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 1 {
		t.Errorf("null modification times did not match: %v", known)
	}
}

// One null and one value is genuinely unknown, so it must fall through to a
// content check rather than be assumed equal.
func TestKnownMappingsRejectsOneSidedNullModificationTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	reported := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	known, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{{LocalID: a.LocalID, ModifiedAt: &reported}})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if len(known) != 0 {
		t.Errorf("stored null matched a reported time: %v", known)
	}
}

func TestAssetsByContentResolvesSingleMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	assetID, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	key := ContentKey{MD5: a.MD5, ByteSize: a.ByteSize}
	found, err := s.AssetsByContent(ctx, []ContentKey{key})
	if err != nil {
		t.Fatalf("AssetsByContent: %v", err)
	}
	match, ok := found[key]
	if !ok {
		t.Fatalf("content key did not resolve: %v", found)
	}
	if match.Matches != 1 {
		t.Errorf("Matches = %d, want 1", match.Matches)
	}
	if match.AssetID != assetID {
		t.Errorf("AssetID = %q, want %q", match.AssetID, assetID)
	}
}

func TestAssetsByContentOmitsUnknownContent(t *testing.T) {
	s := testStore(t)

	found, err := s.AssetsByContent(context.Background(), []ContentKey{
		{MD5: "ffffffffffffffffffffffffffffffff", ByteSize: 99},
	})
	if err != nil {
		t.Fatalf("AssetsByContent: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("unarchived content resolved: %v", found)
	}
}

// Two assets sharing an md5 and a size means md5 is not identifying anything
// here. The caller must be able to see that and re-upload instead of guessing.
func TestAssetsByContentReportsAmbiguousMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, a); err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	collision := sampleAsset()
	collision.SHA256 = "aaaa" + a.SHA256[4:]
	collision.LocalID = "another-local-id"
	if _, _, err := s.RecordAsset(ctx, collision); err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	key := ContentKey{MD5: a.MD5, ByteSize: a.ByteSize}
	found, err := s.AssetsByContent(ctx, []ContentKey{key})
	if err != nil {
		t.Fatalf("AssetsByContent: %v", err)
	}
	if found[key].Matches != 2 {
		t.Errorf("Matches = %d, want 2 so the caller refuses to pick one", found[key].Matches)
	}
}

func TestRecordMappingsMakesContentReachableByLocalID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := sampleAsset()
	assetID, _, err := s.RecordAsset(ctx, a)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	modified := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	err = s.RecordMappings(ctx, a.DeviceID, []Mapping{
		{LocalID: "newly-seen-local-id", AssetID: assetID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("RecordMappings: %v", err)
	}

	known, err := s.KnownMappings(ctx, a.DeviceID, []LocalRef{
		{LocalID: "newly-seen-local-id", ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if known["newly-seen-local-id"] != assetID {
		t.Errorf("recorded mapping did not resolve: %v", known)
	}
}

func TestRecordMappingsOverwritesAStaleMapping(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first := sampleAsset()
	firstID, _, err := s.RecordAsset(ctx, first)
	if err != nil {
		t.Fatalf("first RecordAsset: %v", err)
	}
	edited := sampleAsset()
	edited.SHA256 = "bbbb" + first.SHA256[4:]
	edited.MD5 = "0000d8cd98f00b204e9800998ecf8427"
	edited.LocalID = "throwaway-local-id"
	editedID, _, err := s.RecordAsset(ctx, edited)
	if err != nil {
		t.Fatalf("second RecordAsset: %v", err)
	}

	modified := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	err = s.RecordMappings(ctx, first.DeviceID, []Mapping{
		{LocalID: first.LocalID, AssetID: editedID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("RecordMappings: %v", err)
	}

	known, err := s.KnownMappings(ctx, first.DeviceID, []LocalRef{
		{LocalID: first.LocalID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("KnownMappings: %v", err)
	}
	if known[first.LocalID] == firstID {
		t.Error("mapping still points at the superseded asset")
	}
	if known[first.LocalID] != editedID {
		t.Errorf("mapping = %q, want the newer asset %q", known[first.LocalID], editedID)
	}
}

func TestAssetRoundTripsFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	want := sampleAsset()

	id, _, err := s.RecordAsset(ctx, want)
	if err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}

	got, err := s.Asset(ctx, id)
	if err != nil {
		t.Fatalf("Asset: %v", err)
	}
	if got.SHA256 != want.SHA256 || got.MD5 != want.MD5 || got.ByteSize != want.ByteSize {
		t.Errorf("digest/size lost in round trip: %+v", got)
	}
	if got.OriginalFilename != want.OriginalFilename || got.Ext != want.Ext || got.ContentType != want.ContentType {
		t.Errorf("file metadata lost in round trip: %+v", got)
	}
	if got.DeviceID != want.DeviceID || got.LocalID != want.LocalID {
		t.Errorf("device metadata lost in round trip: %+v", got)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(*want.CapturedAt) {
		t.Errorf("CapturedAt = %v, want %v", got.CapturedAt, want.CapturedAt)
	}
	if got.UploadedAt.IsZero() {
		t.Error("UploadedAt was not populated by the database")
	}
}

func TestAssetReportsNotFoundForUnknownID(t *testing.T) {
	s := testStore(t)

	_, err := s.Asset(context.Background(), "6b3e2c1a-0000-4000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Asset error = %v, want ErrNotFound", err)
	}
}

func TestRecentAssetsOrdersNewestCaptureFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	times := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	for i, ts := range times {
		a := sampleAsset()
		a.SHA256 = string(rune('a'+i)) + sampleAsset().SHA256[1:]
		a.LocalID = a.LocalID + "-" + string(rune('a'+i))
		a.CapturedAt = &ts
		if _, _, err := s.RecordAsset(ctx, a); err != nil {
			t.Fatalf("RecordAsset %d: %v", i, err)
		}
	}

	got, err := s.RecentAssets(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAssets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d assets, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].CapturedAt.Before(*got[i].CapturedAt) {
			t.Errorf("assets out of order at %d: %v before %v", i, got[i-1].CapturedAt, got[i].CapturedAt)
		}
	}
}
