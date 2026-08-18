package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAlbumsAreOrderedByTheirNewestPhoto(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	old := seedAsset(t, s, 1, time.Date(2019, 3, 4, 12, 0, 0, 0, time.UTC))
	recent := seedAsset(t, s, 2, time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, old, heicSidecar, AlbumRef{Title: "Norway 2019"})
	applySidecar(t, s, recent, heicSidecar, AlbumRef{Title: "Iceland 2026"})

	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 2 {
		t.Fatalf("got %d albums, want 2", len(albums))
	}
	if albums[0].Title != "Iceland 2026" {
		t.Errorf("first album is %q, want the one holding the most recent photo", albums[0].Title)
	}
	if albums[0].CoverID != recent {
		t.Errorf("cover is %q, want the album's newest asset %q", albums[0].CoverID, recent)
	}
	if albums[0].Count != 1 {
		t.Errorf("count = %d, want 1", albums[0].Count)
	}
	if !albums[0].NewestAt.Equal(time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("newest = %v, want the cover's capture time", albums[0].NewestAt)
	}
}

// An album whose every photo failed to import is still a fact about the export,
// and one that vanishes silently is one nobody goes looking for.
func TestAnAlbumWithNoPhotosIsStillListed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := seedAsset(t, s, 1, time.Date(2024, 5, 5, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, id, heicSidecar, AlbumRef{Title: "Empty"})
	if _, err := s.pool.Exec(ctx, "delete from album_assets"); err != nil {
		t.Fatalf("clear membership: %v", err)
	}

	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 1 {
		t.Fatalf("got %d albums, want the empty one to survive", len(albums))
	}
	if albums[0].Count != 0 || albums[0].CoverID != "" {
		t.Errorf("empty album reported count=%d cover=%q, want 0 and none",
			albums[0].Count, albums[0].CoverID)
	}
	if !albums[0].NewestAt.IsZero() {
		t.Errorf("newest = %v, want the zero time rather than a date it invented", albums[0].NewestAt)
	}
}

func TestAlbumByIDIsNotFoundForAnUnknownID(t *testing.T) {
	s := testStore(t)

	_, err := s.AlbumByID(context.Background(), "3f2504e0-4f89-11d3-9a0c-0305e82c3301")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("AlbumByID error = %v, want ErrNotFound", err)
	}
}

func TestPeopleAreOrderedByHowOftenTheyAppear(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// heicSidecar names Brody and Dominic; the second photo names only Dominic.
	first := seedAsset(t, s, 1, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC))
	second := seedAsset(t, s, 2, time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, first, heicSidecar)
	applySidecar(t, s, second, `{"people": [{"name": "Dominic"}]}`)

	people, err := s.People(ctx, 10)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	if len(people) != 2 {
		t.Fatalf("got %d people, want 2", len(people))
	}
	if people[0].Name != "Dominic" || people[0].Count != 2 {
		t.Errorf("first is %q with %d photos, want Dominic with 2", people[0].Name, people[0].Count)
	}
	if people[0].CoverID != second {
		t.Errorf("cover is %q, want their most recent photo %q", people[0].CoverID, second)
	}
}

func TestCategoriesCountOnlyWhatTheLibraryHas(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	shot := seedAsset(t, s, 1, time.Date(2025, 2, 2, 12, 0, 0, 0, time.UTC))
	applyPhotoKitSidecar(t, s, shot, `{"favorite": true, "subtypes": ["screenshot"]}`)

	cats, err := s.Categories(ctx)
	if err != nil {
		t.Fatalf("Categories: %v", err)
	}

	by := map[string]Category{}
	for _, c := range cats {
		by[c.Key] = c
	}
	for _, key := range []string{"screenshots", "favorites"} {
		got, ok := by[key]
		if !ok {
			t.Errorf("%s is missing, want it counted", key)
			continue
		}
		if got.Count != 1 || got.CoverID != shot {
			t.Errorf("%s = count %d cover %q, want 1 and %q", key, got.Count, got.CoverID, shot)
		}
	}
	// Nothing in this library is a panorama, and a row saying "Panoramas 0" is
	// worse than no row: it invites a click that leads nowhere.
	if _, ok := by["panoramas"]; ok {
		t.Error("an empty category was listed")
	}
}

func TestTimelineFilteredByAlbumHoldsOnlyItsMembers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	inside := seedAsset(t, s, 1, time.Date(2025, 4, 4, 12, 0, 0, 0, time.UTC))
	seedAsset(t, s, 2, time.Date(2025, 5, 5, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, inside, heicSidecar, AlbumRef{Title: "Iceland 2025"})

	albums, err := s.Albums(ctx)
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}

	page, err := s.Timeline(ctx, TimelineFilter{AlbumID: albums[0].ID}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != inside {
		t.Errorf("album timeline holds %d items, want only %q", len(page.Items), inside)
	}
}

func TestTimelineFilteredByPersonHoldsOnlyTheirPhotos(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	tagged := seedAsset(t, s, 1, time.Date(2025, 4, 4, 12, 0, 0, 0, time.UTC))
	seedAsset(t, s, 2, time.Date(2025, 5, 5, 12, 0, 0, 0, time.UTC))
	applySidecar(t, s, tagged, heicSidecar)

	page, err := s.Timeline(ctx, TimelineFilter{Person: "Brody"}, nil, 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != tagged {
		t.Errorf("person timeline holds %d items, want only %q", len(page.Items), tagged)
	}
}

func TestTimelineFilteredByCategoryPagesLikeTheLibrary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var favorites []string
	for i := range 5 {
		id := seedAsset(t, s, i, time.Date(2025, 1, i+1, 12, 0, 0, 0, time.UTC))
		if i%2 == 0 {
			applyPhotoKitSidecar(t, s, id, `{"favorite": true, "subtypes": []}`)
			favorites = append(favorites, id)
		}
	}

	seen := map[string]bool{}
	var cursor *Cursor
	for range 5 { // bounded so a broken cursor cannot spin forever
		page, err := s.Timeline(ctx, TimelineFilter{Category: "favorites"}, cursor, 2)
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		for _, it := range page.Items {
			if seen[it.ID] {
				t.Errorf("%q was served twice across pages", it.ID)
			}
			seen[it.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		next, err := DecodeCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("DecodeCursor: %v", err)
		}
		cursor = &next
	}

	if len(seen) != len(favorites) {
		t.Errorf("paged through %d favorites, want %d", len(seen), len(favorites))
	}
	for _, id := range favorites {
		if !seen[id] {
			t.Errorf("favorite %q never appeared", id)
		}
	}
}

// A category key the server does not know is a client bug, and answering it
// with the whole library would hide that bug behind a page that looks fine.
func TestTimelineRejectsAnUnknownCategory(t *testing.T) {
	s := testStore(t)

	_, err := s.Timeline(context.Background(), TimelineFilter{Category: "cats"}, nil, 10)
	if !errors.Is(err, ErrUnknownCategory) {
		t.Errorf("Timeline error = %v, want ErrUnknownCategory", err)
	}
}
