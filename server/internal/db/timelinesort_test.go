package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedVideo records a video of a given length, an hour apart from its
// neighbours like seedDays, so a test can state both orders it might be read
// in. The duration arrives the way a real one does — from the worker, after the
// upload — rather than being written into the row directly.
func seedVideo(t *testing.T, s *Store, i int, captured time.Time, seconds float64) string {
	t.Helper()

	a := sampleAsset()
	a.SHA256 = fmt.Sprintf("%064x", i+1)
	a.MD5 = fmt.Sprintf("%032x", i+1)
	a.LocalID = fmt.Sprintf("%s-%d", a.LocalID, i)
	a.OriginalFilename = fmt.Sprintf("IMG_%04d.MOV", i)
	a.Ext = ".mov"
	a.ContentType = "video/quicktime"
	a.MediaKind = MediaVideo
	a.CapturedAt = &captured

	id, _, err := s.RecordAsset(context.Background(), a)
	if err != nil {
		t.Fatalf("RecordAsset %d: %v", i, err)
	}
	if err := s.ApplyMetadata(context.Background(), id, Metadata{
		ExifCapturedAt: &captured, DurationSeconds: &seconds,
	}); err != nil {
		t.Fatalf("ApplyMetadata %d: %v", i, err)
	}
	return id
}

func reversed(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}

// Oldest is the same timeline read backwards, and that is worth stating as a
// test rather than assuming: it is one ORDER BY away from newest, and one
// wrong comparison away from a first page that starts in the middle.
func TestOldestIsTheSameTimelineBackwards(t *testing.T) {
	s := testStore(t)
	ids := seedDays(t, s, 5)

	got := timelineIDs(t, s, TimelineFilter{Sort: SortOldest})
	if want := reversed(ids); !same(got, want) {
		t.Errorf("oldest = %v, want %v", got, want)
	}
}

// The cursor is the half of the ordering that is easy to get wrong, because it
// is a comparison written separately from the ORDER BY it has to agree with.
// Reversed, the two disagree by returning either nothing or everything.
func TestOldestPagesForwardOnACursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := reversed(seedDays(t, s, 5))

	var walked []string
	filter := TimelineFilter{Sort: SortOldest}
	var cursor *Cursor
	for range 5 {
		page, err := s.Timeline(ctx, filter, cursor, 2)
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		for _, item := range page.Items {
			walked = append(walked, item.ID)
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

	if !same(walked, ids) {
		t.Errorf("walked %v, want %v", walked, ids)
	}
}

func TestTimelineDaysFollowTheOrderTheyDescribe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))
	zoned(t, s, 1, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC), minutes(0))
	zoned(t, s, 2, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), minutes(0))

	table, err := s.TimelineDays(ctx, TimelineFilter{Sort: SortOldest}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	want := [][2]any{{"2026-08-04", 1}, {"2026-08-05", 2}}
	if got := runs(table); !equalRuns(got, want) {
		t.Errorf("days = %v, want %v", got, want)
	}
}

// Longest and shortest are the same list read from either end, and the page a
// grid draws has to agree with the count it was laid out from.
func TestDurationOrdersRunFromEitherEnd(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	brief := seedVideo(t, s, 0, base, 3)
	middling := seedVideo(t, s, 1, base.Add(-time.Hour), 42)
	epic := seedVideo(t, s, 2, base.Add(-2*time.Hour), 900)

	longest := timelineIDs(t, s, TimelineFilter{Sort: SortLongest})
	if want := []string{epic, middling, brief}; !same(longest, want) {
		t.Errorf("longest = %v, want %v", longest, want)
	}

	shortest := timelineIDs(t, s, TimelineFilter{Sort: SortShortest})
	if want := []string{brief, middling, epic}; !same(shortest, want) {
		t.Errorf("shortest = %v, want %v", shortest, want)
	}
}

// A photograph has no duration, and "longest first" must not open with every
// still in the archive — which is what `desc` alone would do.
func TestStillsSortLastUnderEitherDurationOrder(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	still := seedAsset(t, s, 0, base)
	clip := seedVideo(t, s, 1, base.Add(-time.Hour), 12)

	for _, sort := range []SortOrder{SortLongest, SortShortest} {
		got := timelineIDs(t, s, TimelineFilter{Sort: sort})
		if want := []string{clip, still}; !same(got, want) {
			t.Errorf("%s = %v, want the video first and %v", sort, got, want)
		}
	}
}

// A timeline ordered by length has no days in it, and the grid is told so: one
// run carrying the whole count and no date. A heading per tile is not a
// description of that shape but a ruin of it.
func TestDurationOrdersHaveNoDays(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	seedVideo(t, s, 0, base, 3)
	seedVideo(t, s, 1, base.Add(-30*time.Hour), 42)

	table, err := s.TimelineDays(ctx, TimelineFilter{Sort: SortLongest}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	if table.Total != 2 {
		t.Fatalf("Total = %d, want 2", table.Total)
	}
	if len(table.Days) != 1 || table.Days[0].Day != "" || table.Days[0].Count != 2 {
		t.Errorf("days = %v, want one headless run of 2", runs(table))
	}
}

// A page with no cursor is what tells the client to keep paging by offset. An
// order that cannot answer a cursor must not hand one out, because the next
// page would be fetched from a different part of the archive entirely.
func TestDurationOrdersHandOutNoCursor(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	seedVideo(t, s, 0, base, 3)
	seedVideo(t, s, 1, base.Add(-time.Hour), 42)

	page, err := s.Timeline(context.Background(), TimelineFilter{Sort: SortLongest}, nil, 1)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want none", page.NextCursor)
	}
	if len(page.Items) != 1 {
		t.Errorf("got %d items, want the page still cut to 1", len(page.Items))
	}
}

func TestKindKeepsOneMediumAndItsCount(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	still := seedAsset(t, s, 0, base)
	clip := seedVideo(t, s, 1, base.Add(-time.Hour), 12)

	if got := timelineIDs(t, s, TimelineFilter{Kind: MediaVideo}); !same(got, []string{clip}) {
		t.Errorf("videos = %v, want %v", got, []string{clip})
	}
	if got := timelineIDs(t, s, TimelineFilter{Kind: MediaImage}); !same(got, []string{still}) {
		t.Errorf("photos = %v, want %v", got, []string{still})
	}

	// The day table is what the grid is laid out from, so it has to be narrowed
	// by the same predicate the pages are.
	table, err := s.TimelineDays(ctx, TimelineFilter{Kind: MediaVideo}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	if table.Total != 1 {
		t.Errorf("Total = %d, want 1", table.Total)
	}
}

func TestAnUnknownKindIsRefusedRatherThanIgnored(t *testing.T) {
	s := testStore(t)
	_, err := s.Timeline(context.Background(), TimelineFilter{Kind: "raw"}, nil, 10)
	if err == nil {
		t.Fatal("Timeline accepted a kind that is neither, want an error")
	}
}

// The facets are adjectives rather than places: they combine with each other
// and with the collection they are asked inside.
func TestFacetsCombineWithEachOtherAndWithACollection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	starred := seedVideo(t, s, 0, base, 20)
	plain := seedVideo(t, s, 1, base.Add(-time.Hour), 30)
	starredStill := seedAsset(t, s, 2, base.Add(-2*time.Hour))

	applyPhotoKitSidecar(t, s, starred, `{"favorite": true, "subtypes": []}`)
	applyPhotoKitSidecar(t, s, starredStill, `{"favorite": true, "subtypes": []}`)

	got := timelineIDs(t, s, TimelineFilter{Favorites: true})
	if want := []string{starred, starredStill}; !same(got, want) {
		t.Errorf("favorites = %v, want %v", got, want)
	}

	got = timelineIDs(t, s, TimelineFilter{Favorites: true, Kind: MediaVideo})
	if want := []string{starred}; !same(got, want) {
		t.Errorf("favorite videos = %v, want %v", got, want)
	}

	album, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if _, err := s.AddToAlbum(ctx, album.ID, Selection{IDs: []string{starred, plain}}); err != nil {
		t.Fatalf("AddToAlbum: %v", err)
	}

	got = timelineIDs(t, s, TimelineFilter{AlbumID: album.ID, Favorites: true})
	if want := []string{starred}; !same(got, want) {
		t.Errorf("favorites in the album = %v, want %v", got, want)
	}
}

// "Not in an album" is the pile left over after the organising, which is the
// whole reason to ask for it — so an album that has been thrown away must not
// go on hiding what was in it.
func TestUnalbumedIgnoresAlbumsThatHaveBeenDeleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	filed := seedAsset(t, s, 0, base)
	loose := seedAsset(t, s, 1, base.Add(-time.Hour))

	album, err := s.CreateAlbum(ctx, NewAlbum{Title: "Iceland"})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if _, err := s.AddToAlbum(ctx, album.ID, Selection{IDs: []string{filed}}); err != nil {
		t.Fatalf("AddToAlbum: %v", err)
	}

	got := timelineIDs(t, s, TimelineFilter{Unalbumed: true})
	if want := []string{loose}; !same(got, want) {
		t.Errorf("unalbumed = %v, want %v", got, want)
	}

	// The album goes, the membership rows stay — that is what makes the undo
	// work — and the photograph is loose again.
	if _, err := s.DeleteAlbum(ctx, album.ID, false); err != nil {
		t.Fatalf("DeleteAlbum: %v", err)
	}
	got = timelineIDs(t, s, TimelineFilter{Unalbumed: true})
	if want := []string{filed, loose}; !same(got, want) {
		t.Errorf("unalbumed after the album went = %v, want %v", got, want)
	}
}

// A selection is runs of positions, and a position means nothing without the
// ordering it was counted in. This is the test that stops a grid sorted
// oldest-first from deleting the newest photographs in the archive.
func TestARangeResolvesInTheOrderItWasCountedIn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := seedDays(t, s, 4)

	filter := TimelineFilter{Sort: SortOldest}
	if _, err := s.Trash(ctx, Selection{
		Ranges: []Range{{Start: 0, End: 2}},
		Filter: filter,
	}); err != nil {
		t.Fatalf("Trash: %v", err)
	}

	// Positions 0 and 1 of an oldest-first grid are the two oldest items, which
	// are the last two in the order everything else here is written in.
	left := timelineIDs(t, s, TimelineFilter{})
	if want := []string{ids[0], ids[1]}; !same(left, want) {
		t.Errorf("left in the library = %v, want %v", left, want)
	}
}

// The position an id maps to is the one the grid would find it at, in whatever
// order that grid is drawn in — including the two that have no keyset to count
// against and are ranked instead.
func TestLocatingAnAssetFollowsTheOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	brief := seedVideo(t, s, 0, base, 3)
	epic := seedVideo(t, s, 1, base.Add(-time.Hour), 900)

	for _, tc := range []struct {
		filter TimelineFilter
		id     string
		want   int
	}{
		{TimelineFilter{}, brief, 0},
		{TimelineFilter{Sort: SortOldest}, brief, 1},
		{TimelineFilter{Sort: SortLongest}, brief, 1},
		{TimelineFilter{Sort: SortShortest}, brief, 0},
		{TimelineFilter{Sort: SortLongest}, epic, 0},
	} {
		at, err := s.TimelinePosition(ctx, tc.filter, tc.id)
		if err != nil {
			t.Fatalf("TimelinePosition %q: %v", tc.filter.Sort, err)
		}
		if at != tc.want {
			t.Errorf("position under %q = %d, want %d", tc.filter.Sort, at, tc.want)
		}
	}
}

// An asset a filter excludes has no position in the timeline that filter
// describes, under either kind of ordering.
func TestRankingSaysNothingAboutWhatTheFilterExcludes(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	still := seedAsset(t, s, 0, base)
	seedVideo(t, s, 1, base.Add(-time.Hour), 12)

	_, err := s.TimelinePosition(context.Background(),
		TimelineFilter{Sort: SortLongest, Kind: MediaVideo}, still)
	if err == nil {
		t.Fatal("TimelinePosition found a photograph in a videos-only timeline")
	}
}

func TestParseSortNamesTheDefaultAndRefusesTheRest(t *testing.T) {
	for _, spelling := range []string{"", "newest"} {
		got, err := ParseSort(spelling)
		if err != nil || got != SortNewest {
			t.Errorf("ParseSort(%q) = %q, %v, want the default", spelling, got, err)
		}
	}
	for _, name := range []string{"oldest", "longest", "shortest"} {
		if got, err := ParseSort(name); err != nil || string(got) != name {
			t.Errorf("ParseSort(%q) = %q, %v, want it accepted", name, got, err)
		}
	}
	if _, err := ParseSort("biggest"); err == nil {
		t.Error("ParseSort accepted an order that does not exist")
	}
}
