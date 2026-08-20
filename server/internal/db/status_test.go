package db

import (
	"context"
	"testing"
)

// digest returns a distinct 64-character sha256 per test asset, since content
// addressing means two assets cannot share one.
func digest(fill string) string {
	out := ""
	for len(out) < 64 {
		out += fill
	}
	return out[:64]
}

// library builds one of everything the status page has to tell apart: a photo,
// a video, the paired video half of a Live Photo, an item in the trash and one
// in the vault.
func library(t *testing.T, s *Store) (photo, video Asset) {
	t.Helper()
	ctx := context.Background()

	photo = sampleAsset()
	photo.ByteSize = 3_000_000
	video = sampleAsset()
	video.SHA256, video.MD5, video.LocalID = digest("1"), digest("1")[:32], "a-video"
	video.MediaKind = MediaVideo
	video.ByteSize = 40_000_000

	// A Live Photo's motion: its own row, its own bytes on disk, and never a
	// tile of its own in the gallery.
	sidecar := sampleAsset()
	sidecar.SHA256, sidecar.MD5, sidecar.LocalID = digest("2"), digest("2")[:32], "a-live-sidecar"
	sidecar.MediaKind = MediaVideo
	sidecar.ByteSize = 1_500_000
	sidecar.LiveParentLocalID = photo.LocalID

	trashed := sampleAsset()
	trashed.SHA256, trashed.MD5, trashed.LocalID = digest("3"), digest("3")[:32], "a-deleted-photo"
	trashed.ByteSize = 2_000_000

	hidden := sampleAsset()
	hidden.SHA256, hidden.MD5, hidden.LocalID = digest("4"), digest("4")[:32], "a-hidden-photo"
	hidden.ByteSize = 9_000_000

	for _, a := range []Asset{photo, video, sidecar, trashed, hidden} {
		if _, _, err := s.RecordAsset(ctx, a); err != nil {
			t.Fatalf("record %s: %v", a.LocalID, err)
		}
	}

	// Written straight in rather than through Trash and the vault service: the
	// queries under test read these two columns, and going the long way round
	// would put a hundred lines of unrelated machinery between the setup and
	// the assertion.
	if _, err := s.pool.Exec(ctx, `update assets set deleted_at = now() where sha256 = $1`, trashed.SHA256); err != nil {
		t.Fatalf("trash an asset: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `update assets set vault = 'hidden' where sha256 = $1`, hidden.SHA256); err != nil {
		t.Fatalf("hide an asset: %v", err)
	}
	return photo, video
}

func TestLibraryStatsCountsWhatTheGalleryDraws(t *testing.T) {
	s := testStore(t)
	library(t, s)

	stats, err := s.LibraryStats(context.Background())
	if err != nil {
		t.Fatalf("LibraryStats: %v", err)
	}
	// The photo and the video. Not the Live Photo's motion, which is part of
	// the photo; not the trashed item; not the hidden one.
	if stats.Items != 2 {
		t.Errorf("items = %d, want 2", stats.Items)
	}
	if stats.Photos != 1 || stats.Videos != 1 {
		t.Errorf("photos = %d, videos = %d; want 1, 1", stats.Photos, stats.Videos)
	}
	if stats.Trashed != 1 {
		t.Errorf("trashed = %d, want 1", stats.Trashed)
	}
}

// Storage is a question about the disk, so it counts everything occupying it —
// including the components the gallery hides and the items in the trash, which
// are still there for a year.
func TestStoredBytesCountsComponentsAndTheTrash(t *testing.T) {
	s := testStore(t)
	photo, video := library(t, s)

	bytes, err := s.StoredBytes(context.Background())
	if err != nil {
		t.Fatalf("StoredBytes: %v", err)
	}
	if want := photo.ByteSize + 2_000_000; bytes.Photos != want {
		t.Errorf("photo bytes = %d, want %d (the photo and the trashed one)", bytes.Photos, want)
	}
	if want := video.ByteSize + 1_500_000; bytes.Videos != want {
		t.Errorf("video bytes = %d, want %d (the video and the live sidecar)", bytes.Videos, want)
	}
}

// The vault's whole point is that nothing without the password can say what is
// in it, and its size is part of what is in it.
func TestStoredBytesLeavesTheVaultOut(t *testing.T) {
	s := testStore(t)
	library(t, s)

	bytes, err := s.StoredBytes(context.Background())
	if err != nil {
		t.Fatalf("StoredBytes: %v", err)
	}
	if bytes.Photos >= 9_000_000 {
		t.Errorf("photo bytes = %d; the 9MB hidden photo was counted", bytes.Photos)
	}
}

func TestMediaKindsNamesEveryUnvaultedDigest(t *testing.T) {
	s := testStore(t)
	photo, video := library(t, s)

	kinds, err := s.MediaKinds(context.Background())
	if err != nil {
		t.Fatalf("MediaKinds: %v", err)
	}
	if kinds[photo.SHA256] != string(MediaImage) {
		t.Errorf("photo kind = %q, want image", kinds[photo.SHA256])
	}
	if kinds[video.SHA256] != string(MediaVideo) {
		t.Errorf("video kind = %q, want video", kinds[video.SHA256])
	}
	// Unknown rather than absent from the tree: its renditions are on the disk
	// and have to be counted as something.
	if _, ok := kinds[digest("4")]; ok {
		t.Error("the hidden photo's digest was classified; its derivatives should stay unattributed")
	}
}

func TestAssetLabelsWithholdsTheVaultAndSkipsWhatIsGone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	photo, _ := library(t, s)

	var photoID, hiddenID string
	if err := s.pool.QueryRow(ctx, `select id::text from assets where sha256 = $1`, photo.SHA256).Scan(&photoID); err != nil {
		t.Fatalf("look up photo id: %v", err)
	}
	if err := s.pool.QueryRow(ctx, `select id::text from assets where sha256 = $1`, digest("4")).Scan(&hiddenID); err != nil {
		t.Fatalf("look up hidden id: %v", err)
	}
	const purged = "00000000-0000-0000-0000-000000000000"

	labels, err := s.AssetLabels(ctx, []string{photoID, hiddenID, purged})
	if err != nil {
		t.Fatalf("AssetLabels: %v", err)
	}
	if got := labels[photoID]; got.Filename != photo.OriginalFilename || !got.Viewable {
		t.Errorf("photo label = %+v, want %q and viewable", got, photo.OriginalFilename)
	}
	if got := labels[hiddenID]; got.Filename != "" || got.Viewable {
		t.Errorf("hidden label = %+v, want no filename and not viewable", got)
	}
	// A job outlives the asset it was queued for. The failure is still worth
	// reporting, so this is an absence rather than an error.
	if _, ok := labels[purged]; ok {
		t.Error("an id with no row was labelled")
	}
}
