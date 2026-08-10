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

func TestInsertAssetReturnsNewID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, inserted, err := s.InsertAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("InsertAsset: %v", err)
	}
	if !inserted {
		t.Errorf("inserted = false, want true for a new sha256")
	}
	if id == "" {
		t.Error("InsertAsset returned an empty id")
	}
}

func TestInsertAssetIsIdempotentOnSHA256(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	firstID, _, err := s.InsertAsset(ctx, sampleAsset())
	if err != nil {
		t.Fatalf("first InsertAsset: %v", err)
	}

	// Same bytes arriving again from a different local asset must not create a
	// second row; the archive is addressed by content, not by phone identity.
	dup := sampleAsset()
	dup.LocalID = "a-different-local-id"
	secondID, inserted, err := s.InsertAsset(ctx, dup)
	if err != nil {
		t.Fatalf("second InsertAsset: %v", err)
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

func TestAssetRoundTripsFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	want := sampleAsset()

	id, _, err := s.InsertAsset(ctx, want)
	if err != nil {
		t.Fatalf("InsertAsset: %v", err)
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
		a.CapturedAt = &ts
		if _, _, err := s.InsertAsset(ctx, a); err != nil {
			t.Fatalf("InsertAsset %d: %v", i, err)
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
