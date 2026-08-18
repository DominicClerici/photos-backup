package db

import (
	"context"
	"testing"
	"time"
)

func TestDeviceStatsEmptyForADeviceThatHasSentNothing(t *testing.T) {
	s := testStore(t)

	stats, err := s.DeviceStats(context.Background(), "a-device-with-no-assets")
	if err != nil {
		t.Fatalf("DeviceStats: %v", err)
	}
	if stats.Archived != 0 || stats.Bytes != 0 {
		t.Errorf("archived = %d, bytes = %d; want 0, 0", stats.Archived, stats.Bytes)
	}
	// Null rather than the zero time: a phone that has backed up nothing has no
	// last backup, and rendering 1 January year 1 would be worse than rendering
	// nothing at all.
	if stats.LastUploadAt != nil {
		t.Errorf("LastUploadAt = %v, want nil", stats.LastUploadAt)
	}
}

func TestDeviceStatsSplitsPhotosFromVideos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	photo := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, photo); err != nil {
		t.Fatalf("record photo: %v", err)
	}

	video := sampleAsset()
	video.SHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	video.MD5 = "11111111111111111111111111111111"
	video.LocalID = "a-video"
	video.ByteSize = 40_000_000
	video.MediaKind = MediaVideo
	if _, _, err := s.RecordAsset(ctx, video); err != nil {
		t.Fatalf("record video: %v", err)
	}

	stats, err := s.DeviceStats(ctx, photo.DeviceID)
	if err != nil {
		t.Fatalf("DeviceStats: %v", err)
	}
	if stats.Archived != 2 {
		t.Errorf("archived = %d, want 2", stats.Archived)
	}
	if want := photo.ByteSize + video.ByteSize; stats.Bytes != want {
		t.Errorf("bytes = %d, want %d", stats.Bytes, want)
	}
	if stats.Photos != 1 || stats.Videos != 1 {
		t.Errorf("photos = %d, videos = %d; want 1, 1", stats.Photos, stats.Videos)
	}
	if stats.LastUploadAt == nil {
		t.Error("LastUploadAt = nil, want the time the mappings were recorded")
	}
}

// The archive stores one copy of duplicated content, but a phone holding the
// same photo under two local ids has two items and considers both backed up.
// The device block counts what the phone has; the archive block counts disk.
func TestStatsCountDuplicateContentDifferently(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, first); err != nil {
		t.Fatalf("record first: %v", err)
	}
	second := sampleAsset()
	second.LocalID = "the-same-photo-saved-twice"
	if _, inserted, err := s.RecordAsset(ctx, second); err != nil {
		t.Fatalf("record duplicate: %v", err)
	} else if inserted {
		t.Fatal("the duplicate created a second asset row; this test proves nothing")
	}

	device, err := s.DeviceStats(ctx, first.DeviceID)
	if err != nil {
		t.Fatalf("DeviceStats: %v", err)
	}
	if device.Archived != 2 {
		t.Errorf("device archived = %d, want 2 (one per local item)", device.Archived)
	}
	if want := first.ByteSize * 2; device.Bytes != want {
		t.Errorf("device bytes = %d, want %d (summed per mapping)", device.Bytes, want)
	}

	archive, err := s.ArchiveStats(ctx)
	if err != nil {
		t.Fatalf("ArchiveStats: %v", err)
	}
	if archive.Assets != 1 {
		t.Errorf("archive assets = %d, want 1 (content-addressed)", archive.Assets)
	}
	if archive.Bytes != first.ByteSize {
		t.Errorf("archive bytes = %d, want %d (the disk actually spent)", archive.Bytes, first.ByteSize)
	}
}

func TestDeviceStatsAreScopedToOneDevice(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mine := sampleAsset()
	if _, _, err := s.RecordAsset(ctx, mine); err != nil {
		t.Fatalf("record mine: %v", err)
	}

	theirs := sampleAsset()
	theirs.SHA256 = "2222222222222222222222222222222222222222222222222222222222222222"
	theirs.MD5 = "22222222222222222222222222222222"
	theirs.DeviceID = "somebody-elses-phone"
	theirs.LocalID = "their-local-id"
	if _, _, err := s.RecordAsset(ctx, theirs); err != nil {
		t.Fatalf("record theirs: %v", err)
	}

	stats, err := s.DeviceStats(ctx, mine.DeviceID)
	if err != nil {
		t.Fatalf("DeviceStats: %v", err)
	}
	if stats.Archived != 1 {
		t.Errorf("archived = %d, want 1; the other phone's asset leaked in", stats.Archived)
	}

	archive, err := s.ArchiveStats(ctx)
	if err != nil {
		t.Fatalf("ArchiveStats: %v", err)
	}
	if archive.Assets != 2 {
		t.Errorf("archive assets = %d, want 2; the archive is every device's", archive.Assets)
	}
}

// A content match records a mapping without an upload, and it is still the last
// time this phone got something archived — so it has to move LastUploadAt.
// Reading assets.uploaded_at instead would report whenever the other device
// happened to send those bytes.
func TestDeviceStatsCountAssetsThisDeviceNeverUploaded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	uploaded := sampleAsset()
	uploaded.DeviceID = "the-phone-that-sent-it"
	assetID, _, err := s.RecordAsset(ctx, uploaded)
	if err != nil {
		t.Fatalf("record asset: %v", err)
	}

	const second = "the-phone-that-already-had-it"
	modified := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	err = s.RecordMappings(ctx, second, []Mapping{
		{LocalID: "its-own-local-id", AssetID: assetID, ModifiedAt: &modified},
	})
	if err != nil {
		t.Fatalf("RecordMappings: %v", err)
	}

	stats, err := s.DeviceStats(ctx, second)
	if err != nil {
		t.Fatalf("DeviceStats: %v", err)
	}
	if stats.Archived != 1 {
		t.Errorf("archived = %d, want 1; a content match is still backed up", stats.Archived)
	}
	if stats.LastUploadAt == nil {
		t.Error("LastUploadAt = nil; the mapping is when this phone got it archived")
	}
}
