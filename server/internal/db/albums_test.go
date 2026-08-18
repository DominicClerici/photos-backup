package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateAlbumIsEmptyAndListed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	made, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland 2026", Description: "Three weeks"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if made.ID == "" {
		t.Fatal("CreateAlbum returned no id")
	}
	if made.Source != GallerySource {
		t.Errorf("source = %q, want the gallery's own", made.Source)
	}

	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 1 {
		t.Fatalf("got %d albums, want the new one", len(albums))
	}
	if albums[0].Count != 0 || albums[0].CoverID != "" {
		t.Errorf("a new album reported count=%d cover=%q, want empty",
			albums[0].Count, albums[0].CoverID)
	}
	if albums[0].Description != "Three weeks" {
		t.Errorf("description = %q, want what was typed", albums[0].Description)
	}
}

func TestCreateAlbumRefusesANameThatIsTaken(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland"}); err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	_, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland"})
	if !errors.Is(err, ErrDuplicateAlbum) {
		t.Fatalf("second create returned %v, want ErrDuplicateAlbum", err)
	}
}

// An import's "Favorites" and a hand-made "Favorites" are two albums, because
// they came from two places and merging them would silently rewrite one.
func TestAnImportedNameDoesNotBlockAHandMadeOne(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2024, 5, 5, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Favorites"})

	if _, err := s.CreateAlbum(ctx, NewAlbum{Title: "Favorites"}); err != nil {
		t.Fatalf("CreateAlbum alongside an imported album of the same name: %v", err)
	}
}

// The name is a name among live albums. Deleting one releases it — see
// migration 0013 — because a title held hostage by something in Recently
// Deleted is a refusal nobody can act on.
func TestADeletedAlbumReleasesItsName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	made, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if _, err := s.DeleteAlbum(ctx, made.ID, false); err != nil {
		t.Fatalf("DeleteAlbum: %v", err)
	}
	if _, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland"}); err != nil {
		t.Fatalf("CreateAlbum after deleting the old one: %v", err)
	}
}

func TestAddAndRemoveMembership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	one := seedAsset(t, s, 1, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	two := seedAsset(t, s, 2, time.Date(2024, 2, 2, 12, 0, 0, 0, time.UTC))

	album, err := s.CreateAlbum(ctx, NewAlbum{Title: "Trip"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}

	added, err := s.AddToAlbum(ctx, album.ID, Selection{IDs: []string{one, two}})
	if err != nil {
		t.Fatalf("AddToAlbum: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}

	// Adding what is already in there is not an error and is not a second
	// membership; the count says so rather than the toast lying about it.
	again, err := s.AddToAlbum(ctx, album.ID, Selection{IDs: []string{one, two}})
	if err != nil {
		t.Fatalf("AddToAlbum twice: %v", err)
	}
	if again != 0 {
		t.Errorf("re-adding reported %d, want 0", again)
	}

	held, err := s.AlbumIDsOf(ctx, one)
	if err != nil {
		t.Fatalf("AlbumIDsOf: %v", err)
	}
	if len(held) != 1 || held[0] != album.ID {
		t.Errorf("AlbumIDsOf = %v, want just %s", held, album.ID)
	}

	removed, err := s.RemoveFromAlbum(ctx, album.ID, Selection{IDs: []string{one}})
	if err != nil {
		t.Fatalf("RemoveFromAlbum: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	// The photograph is still in the library. Removing it from an album takes
	// away the grouping and nothing else.
	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("timeline holds %d items, want both photos still in the library", len(page.Items))
	}
}

// Positions in a selection are counted in the timeline the client was looking
// at, which for "remove these from this album" is the album itself.
func TestRemoveByRangeCountsPositionsInsideTheAlbum(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	outside := seedAsset(t, s, 1, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	inside := seedAsset(t, s, 2, time.Date(2024, 2, 2, 12, 0, 0, 0, time.UTC))

	album, err := s.CreateAlbum(ctx, NewAlbum{Title: "Trip"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if _, err := s.AddToAlbum(ctx, album.ID, Selection{IDs: []string{inside}}); err != nil {
		t.Fatalf("AddToAlbum: %v", err)
	}

	// Index 0 of the library is `outside`, which is newer. Index 0 of the album
	// is the only thing in it.
	removed, err := s.RemoveFromAlbum(ctx, album.ID, Selection{
		Ranges: []Range{{Start: 0, End: 1}},
		Filter: TimelineFilter{AlbumID: album.ID},
	})
	if err != nil {
		t.Fatalf("RemoveFromAlbum: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want the album's own first item", removed)
	}
	if held, err := s.AlbumIDsOf(ctx, outside); err != nil || len(held) != 0 {
		t.Errorf("the library's first item was touched: %v %v", held, err)
	}
}

func TestAlbumHomeReportsTheBucket(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	made, err := s.CreateAlbum(ctx, NewAlbum{Title: "Private", Vault: VaultHidden})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	_, bucket, err := s.AlbumHome(ctx, made.ID)
	if err != nil {
		t.Fatalf("AlbumHome: %v", err)
	}
	if bucket != VaultHidden {
		t.Errorf("bucket = %q, want %q", bucket, VaultHidden)
	}

	// And it is not on the collections page, which draws the library only.
	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 0 {
		t.Errorf("an album made inside a bucket is listed in the library: %v", albums)
	}

	hidden, err := s.VaultedAlbums(ctx, VaultHidden)
	if err != nil {
		t.Fatalf("VaultedAlbums: %v", err)
	}
	if len(hidden) != 1 || hidden[0].Title != "Private" {
		t.Errorf("VaultedAlbums = %v, want the new album", hidden)
	}
}

func TestAlbumHomeRejectsAnAlbumThatIsGone(t *testing.T) {
	s := testStore(t)

	if _, _, err := s.AlbumHome(context.Background(),
		"00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AlbumHome for a missing album returned %v, want ErrNotFound", err)
	}
}
