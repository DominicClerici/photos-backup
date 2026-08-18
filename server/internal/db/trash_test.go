package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedDays records n assets an hour apart, newest first, and returns their ids
// in timeline order — so ids[0] is at position 0 and a Range over positions is
// something a test can state directly.
func seedDays(t *testing.T, s *Store, n int) []string {
	t.Helper()
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	ids := make([]string, n)
	for i := range n {
		ids[i] = seedAsset(t, s, i, base.Add(time.Duration(-i)*time.Hour))
	}
	return ids
}

// ids reads back the timeline as a list of ids, which is the shape almost every
// assertion here wants.
func timelineIDs(t *testing.T, s *Store, filter TimelineFilter) []string {
	t.Helper()
	page, err := s.Timeline(context.Background(), filter, nil, 500)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.ID)
	}
	return out
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTrashByRangeTakesTheSelectedPositions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 5)

	result, err := s.Trash(ctx, Selection{Ranges: []Range{{Start: 1, End: 3}}})
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if result.Count != 2 {
		t.Errorf("Count = %d, want 2", result.Count)
	}
	if result.Batch == "" {
		t.Error("Batch is empty; there would be nothing to undo with")
	}

	if got, want := timelineIDs(t, s, TimelineFilter{}), []string{ids[0], ids[3], ids[4]}; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}
	if got, want := timelineIDs(t, s, TimelineFilter{Trash: true}), []string{ids[1], ids[2]}; !same(got, want) {
		t.Errorf("trash = %v, want %v", got, want)
	}
}

// The positions a range names are positions in the *filtered* timeline. A
// selection made inside an album has to mean the album's second photo, not the
// library's — the grid is drawn from the album's day table and knows nothing
// about where those photographs sit in the whole archive.
func TestTrashByRangeCountsInsideTheFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 4)

	if err := s.ApplyImportMetadata(ctx, ids[3], ImportMetadata{
		Source: "test", Albums: []AlbumRef{{Title: "Iceland"}},
	}); err != nil {
		t.Fatalf("put ids[3] in an album: %v", err)
	}
	albums, err := s.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("Albums = %v, %v", albums, err)
	}

	// Position 0 of the album, which is position 3 of the library.
	if _, err := s.Trash(ctx, Selection{
		Ranges: []Range{{Start: 0, End: 1}},
		Filter: TimelineFilter{AlbumID: albums[0].ID},
	}); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	if got, want := timelineIDs(t, s, TimelineFilter{}), ids[:3]; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}
}

func TestTrashByIDIsExact(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 3)

	result, err := s.Trash(ctx, Selection{IDs: []string{ids[1]}})
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if result.Count != 1 {
		t.Errorf("Count = %d, want 1", result.Count)
	}
	if got, want := timelineIDs(t, s, TimelineFilter{}), []string{ids[0], ids[2]}; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}
}

// A still and its motion are one photograph. Deleting the still has to take the
// video with it, or the archive keeps three seconds of a moment it no longer
// holds — and restoring the still has to bring it back, or the photo comes back
// dead.
func TestTrashCarriesTheLivePairAndRestoreBringsItBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	still, video := livePair(t, 0)
	stillID, _, err := s.RecordAsset(ctx, still)
	if err != nil {
		t.Fatalf("record still: %v", err)
	}
	videoID, _, err := s.RecordAsset(ctx, video)
	if err != nil {
		t.Fatalf("record paired video: %v", err)
	}

	result, err := s.Trash(ctx, Selection{IDs: []string{stillID}})
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	// One item, two rows: the video was never something anybody could select.
	if result.Count != 1 {
		t.Errorf("Count = %d, want 1", result.Count)
	}

	if a, err := s.Asset(ctx, videoID); err != nil {
		t.Fatalf("load paired video: %v", err)
	} else if a.DeletedAt == nil {
		t.Error("the paired video is still live after its still was deleted")
	}

	assets, albums, err := s.RestoreBatch(ctx, result.Batch)
	if err != nil {
		t.Fatalf("RestoreBatch: %v", err)
	}
	if assets != 1 || albums != 0 {
		t.Errorf("RestoreBatch = %d assets, %d albums; want 1, 0", assets, albums)
	}
	if a, err := s.Asset(ctx, videoID); err != nil {
		t.Fatalf("load paired video: %v", err)
	} else if a.DeletedAt != nil {
		t.Error("the paired video stayed in the trash after its still was restored")
	}
}

// The batch is the undo, so it has to name what that one operation did and
// nothing else — not something already in the trash when it ran, which the undo
// would otherwise silently pull back out.
func TestRestoreBatchLeavesEarlierDeletesAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 3)

	if _, err := s.Trash(ctx, Selection{IDs: []string{ids[0]}}); err != nil {
		t.Fatalf("first Trash: %v", err)
	}
	second, err := s.Trash(ctx, Selection{IDs: []string{ids[0], ids[1]}})
	if err != nil {
		t.Fatalf("second Trash: %v", err)
	}
	if second.Count != 1 {
		t.Errorf("Count = %d, want 1: only one of the two was still live", second.Count)
	}

	if _, _, err := s.RestoreBatch(ctx, second.Batch); err != nil {
		t.Fatalf("RestoreBatch: %v", err)
	}
	if got, want := timelineIDs(t, s, TimelineFilter{}), []string{ids[1], ids[2]}; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}
}

func TestRestoreByRangeCountsInsideTheTrash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 4)

	if _, err := s.Trash(ctx, Selection{Ranges: []Range{{Start: 0, End: 4}}}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	// Positions 1 and 2 of the trash, which is the same ordering the library
	// had — the trash is the same timeline over the other half of a boolean.
	n, err := s.Restore(ctx, Selection{Ranges: []Range{{Start: 1, End: 3}}})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 2 {
		t.Errorf("Restore = %d, want 2", n)
	}
	if got, want := timelineIDs(t, s, TimelineFilter{}), []string{ids[1], ids[2]}; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}
}

// A delete can only ever name something live and a restore only something
// deleted. Which means the two cannot reach each other's half of the archive
// even when handed an id from it.
func TestTrashRefusesWhatIsAlreadyDeleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 2)

	if _, err := s.Trash(ctx, Selection{IDs: []string{ids[0]}}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	again, err := s.Trash(ctx, Selection{IDs: []string{ids[0]}})
	if err != nil {
		t.Fatalf("second Trash: %v", err)
	}
	if again.Count != 0 {
		t.Errorf("Count = %d, want 0", again.Count)
	}

	n, err := s.Restore(ctx, Selection{IDs: []string{ids[1]}})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 0 {
		t.Errorf("Restore = %d, want 0: that asset was never deleted", n)
	}
}

func TestSelectionWithNothingInItIsRefused(t *testing.T) {
	s := testStore(t)
	if _, err := s.Trash(context.Background(), Selection{}); !errors.Is(err, ErrEmptySelection) {
		t.Errorf("Trash of an empty selection = %v, want ErrEmptySelection", err)
	}
}

func TestTrashedAssetsLeaveTheirCollections(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 2)

	for _, id := range ids {
		if err := s.ApplyImportMetadata(ctx, id, ImportMetadata{
			Source: "test",
			Albums: []AlbumRef{{Title: "Iceland"}},
			People: []string{"Ada"},
		}); err != nil {
			t.Fatalf("import metadata: %v", err)
		}
	}

	if _, err := s.Trash(ctx, Selection{IDs: []string{ids[0]}}); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	albums, err := s.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("Albums = %v, %v", albums, err)
	}
	if albums[0].Count != 1 {
		t.Errorf("album count = %d, want 1", albums[0].Count)
	}
	people, err := s.People(ctx, 10)
	if err != nil || len(people) != 1 {
		t.Fatalf("People = %v, %v", people, err)
	}
	if people[0].Count != 1 {
		t.Errorf("person count = %d, want 1", people[0].Count)
	}

	n, err := s.TrashCount(ctx)
	if err != nil {
		t.Fatalf("TrashCount: %v", err)
	}
	if n != 1 {
		t.Errorf("TrashCount = %d, want 1", n)
	}
}

func TestDeleteAlbumKeepsItsPhotos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 2)

	for _, id := range ids {
		if err := s.ApplyImportMetadata(ctx, id, ImportMetadata{
			Source: "test", Albums: []AlbumRef{{Title: "Iceland"}},
		}); err != nil {
			t.Fatalf("import metadata: %v", err)
		}
	}
	albums, err := s.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("Albums = %v, %v", albums, err)
	}

	result, err := s.DeleteAlbum(ctx, albums[0].ID, false)
	if err != nil {
		t.Fatalf("DeleteAlbum: %v", err)
	}
	if result.Count != 0 {
		t.Errorf("Count = %d, want 0: the photos were not deleted", result.Count)
	}

	if got, err := s.Albums(ctx); err != nil || len(got) != 0 {
		t.Errorf("Albums = %v, %v; want none", got, err)
	}
	if _, err := s.AlbumByID(ctx, albums[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("AlbumByID of a deleted album = %v, want ErrNotFound", err)
	}
	if got, want := timelineIDs(t, s, TimelineFilter{}), ids; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}

	if _, albums, err := s.RestoreBatch(ctx, result.Batch); err != nil {
		t.Fatalf("RestoreBatch: %v", err)
	} else if albums != 1 {
		t.Errorf("RestoreBatch = %d albums, want 1", albums)
	}
	if got, err := s.Albums(ctx); err != nil || len(got) != 1 {
		t.Errorf("Albums after undo = %v, %v; want one", got, err)
	}
}

func TestDeleteAlbumWithItsPhotosUndoesAsOne(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 3)

	for _, id := range ids[:2] {
		if err := s.ApplyImportMetadata(ctx, id, ImportMetadata{
			Source: "test", Albums: []AlbumRef{{Title: "Iceland"}},
		}); err != nil {
			t.Fatalf("import metadata: %v", err)
		}
	}
	albums, err := s.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("Albums = %v, %v", albums, err)
	}

	result, err := s.DeleteAlbum(ctx, albums[0].ID, true)
	if err != nil {
		t.Fatalf("DeleteAlbum: %v", err)
	}
	if result.Count != 2 {
		t.Errorf("Count = %d, want 2", result.Count)
	}
	if got, want := timelineIDs(t, s, TimelineFilter{}), []string{ids[2]}; !same(got, want) {
		t.Errorf("library = %v, want %v", got, want)
	}

	assets, restored, err := s.RestoreBatch(ctx, result.Batch)
	if err != nil {
		t.Fatalf("RestoreBatch: %v", err)
	}
	if assets != 2 || restored != 1 {
		t.Errorf("RestoreBatch = %d assets, %d albums; want 2, 1", assets, restored)
	}
	if got := timelineIDs(t, s, TimelineFilter{}); !same(got, ids) {
		t.Errorf("library after undo = %v, want %v", got, ids)
	}
}

func TestDeleteAlbumThatIsNotThere(t *testing.T) {
	s := testStore(t)
	_, err := s.DeleteAlbum(context.Background(),
		"00000000-0000-4000-8000-000000000000", false)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteAlbum of a missing album = %v, want ErrNotFound", err)
	}
}

func TestPurgeRemovesTheRowsAndTombstonesTheContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 3)

	if _, err := s.Trash(ctx, Selection{IDs: ids[:2]}); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	gone, err := s.Purge(ctx, Selection{IDs: ids[:2]})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if PurgedItems(gone) != 2 {
		t.Errorf("purged %d items, want 2", PurgedItems(gone))
	}
	for _, p := range gone {
		if p.SHA256 == "" || p.Ext == "" {
			t.Errorf("purged asset %s has nothing to find its blob with: %+v", p.ID, p)
		}
	}

	if _, err := s.Asset(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Errorf("the purged row is still there: %v", err)
	}

	key := ContentKey{MD5: gone[0].MD5, ByteSize: gone[0].ByteSize}
	purged, err := s.PurgedContent(ctx, []ContentKey{key})
	if err != nil {
		t.Fatalf("PurgedContent: %v", err)
	}
	if !purged[key] {
		t.Error("the purged content is not tombstoned; the next backup would upload it again")
	}
}

// The purge is the one operation with no undo, so the trash is not a step it
// can skip.
func TestPurgeCannotReachTheLibrary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 2)

	gone, err := s.Purge(ctx, Selection{IDs: []string{ids[0]}})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if len(gone) != 0 {
		t.Errorf("purged %d rows from the live library, want 0", len(gone))
	}
	if _, err := s.Asset(ctx, ids[0]); err != nil {
		t.Errorf("the asset is gone: %v", err)
	}
}

func TestPurgeExpiredTakesOnlyWhatIsDue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 3)

	if _, err := s.Trash(ctx, Selection{IDs: ids[:2]}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	// One of the two is a year old; the other went in today.
	if _, err := s.pool.Exec(ctx,
		`update assets set purge_after = now() - interval '1 day' where id = $1::uuid`,
		ids[0]); err != nil {
		t.Fatalf("age the first delete: %v", err)
	}

	gone, err := s.PurgeExpired(ctx, 100)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if len(gone) != 1 || gone[0].ID != ids[0] {
		t.Fatalf("purged %v, want just %s", gone, ids[0])
	}
	if got, want := timelineIDs(t, s, TimelineFilter{Trash: true}), []string{ids[1]}; !same(got, want) {
		t.Errorf("trash = %v, want %v", got, want)
	}
}
