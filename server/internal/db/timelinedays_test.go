package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// zoned records an asset captured at the given instant, carrying the UTC offset
// the file itself recorded. A nil offset is a file that recorded none, which is
// the case the viewer's timezone has to answer for.
func zoned(t *testing.T, s *Store, i int, at time.Time, offsetMinutes *int) string {
	t.Helper()
	id := seedAsset(t, s, i, at)
	if err := s.ApplyMetadata(context.Background(), id, Metadata{
		ExifCapturedAt: &at, ExifOffsetMinutes: offsetMinutes,
	}); err != nil {
		t.Fatalf("ApplyMetadata %d: %v", i, err)
	}
	return id
}

func minutes(n int) *int { return &n }

// runs renders a day table as the pairs a test can read.
func runs(table DayTable) [][2]any {
	out := make([][2]any, 0, len(table.Days))
	for _, d := range table.Days {
		out = append(out, [2]any{d.Day, d.Count})
	}
	return out
}

func TestTimelineDaysCountsEachHeadingsTiles(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))
	zoned(t, s, 1, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC), minutes(0))
	zoned(t, s, 2, time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC), minutes(0))
	zoned(t, s, 3, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), minutes(0))

	table, err := s.TimelineDays(ctx, TimelineFilter{}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	want := [][2]any{{"2026-08-05", 3}, {"2026-08-04", 1}}
	if got := runs(table); !equalRuns(got, want) {
		t.Errorf("days = %v, want %v", got, want)
	}
	if table.Total != 4 {
		t.Errorf("Total = %d, want 4", table.Total)
	}
}

// A date is not a heading. Items are ordered by instant and filed under their
// own local day, so a photo taken either side of a timezone hop puts a date on
// both sides of another one — and the grid draws that date twice. A group-by
// would report one heading of two items, which is a shape the timeline does not
// have and would put every tile after it in the wrong place.
func TestTimelineDaysSplitsADateThatRecurs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	zoned(t, s, 0, time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC), minutes(0))
	// 21:00 the previous evening in Vermont, taken between the two UTC photos.
	zoned(t, s, 1, time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC), minutes(-240))
	zoned(t, s, 2, time.Date(2026, 8, 5, 0, 30, 0, 0, time.UTC), minutes(0))

	table, err := s.TimelineDays(ctx, TimelineFilter{}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	want := [][2]any{{"2026-08-05", 1}, {"2026-08-04", 1}, {"2026-08-05", 1}}
	if got := runs(table); !equalRuns(got, want) {
		t.Errorf("days = %v, want %v", got, want)
	}
}

// The file's own offset decides, and only a file that recorded none falls back
// to where the viewer happens to be sitting.
func TestTimelineDaysUsesTheViewersZoneOnlyAsAFallback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// 23:50 in Vermont, which is 03:50 the next day in UTC.
	zoned(t, s, 0, time.Date(2026, 8, 6, 3, 50, 0, 0, time.UTC), minutes(-240))
	// The same instant an hour earlier, from a file that recorded no zone.
	zoned(t, s, 1, time.Date(2026, 8, 6, 2, 50, 0, 0, time.UTC), nil)

	inNewYork, err := s.TimelineDays(ctx, TimelineFilter{}, "America/New_York")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	want := [][2]any{{"2026-08-05", 2}}
	if got := runs(inNewYork); !equalRuns(got, want) {
		t.Errorf("in New York days = %v, want %v", got, want)
	}

	inUTC, err := s.TimelineDays(ctx, TimelineFilter{}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	// The zoned file does not move; only the one with nothing to go on does,
	// and it moves forward past a photo taken after it — which is how a run of
	// days ends up not being in descending date order at all.
	wantUTC := [][2]any{{"2026-08-05", 1}, {"2026-08-06", 1}}
	if got := runs(inUTC); !equalRuns(got, wantUTC) {
		t.Errorf("in UTC days = %v, want %v", got, wantUTC)
	}
}

// The whole point of the table: run lengths sum to positions in the very
// ordering the timeline pages through. If they ever disagree, the grid draws
// photos under headings they do not belong to.
func TestTimelineDaysAgreesWithTheTimelineItDescribes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 20, 0, 0, 0, time.UTC)
	for i := range 25 {
		zoned(t, s, i, base.Add(-time.Duration(i)*time.Hour), minutes(0))
	}

	table, err := s.TimelineDays(ctx, TimelineFilter{}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	page, err := s.Timeline(ctx, TimelineFilter{}, nil, 500)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if table.Total != len(page.Items) {
		t.Fatalf("Total = %d, timeline served %d", table.Total, len(page.Items))
	}

	at := 0
	for _, day := range table.Days {
		for n := range day.Count {
			it := page.Items[at+n]
			if got := it.TakenAt.UTC().Format("2006-01-02"); got != day.Day {
				t.Errorf("item %d is from %s, but sits under the %s heading",
					at+n, got, day.Day)
			}
		}
		at += day.Count
	}
}

func TestTimelineDaysNarrowsToACollection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))
	video := zoned(t, s, 1, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), minutes(0))
	if _, err := s.pool.Exec(ctx,
		`update assets set media_kind = 'video' where id = $1`, video); err != nil {
		t.Fatalf("mark video: %v", err)
	}

	table, err := s.TimelineDays(ctx, TimelineFilter{Category: "videos"}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	want := [][2]any{{"2026-08-04", 1}}
	if got := runs(table); !equalRuns(got, want) {
		t.Errorf("days = %v, want %v", got, want)
	}
	if table.Total != 1 {
		t.Errorf("Total = %d, want 1", table.Total)
	}
}

func TestTimelineDaysOnAnEmptyArchive(t *testing.T) {
	table, err := testStore(t).TimelineDays(context.Background(), TimelineFilter{}, "UTC")
	if err != nil {
		t.Fatalf("TimelineDays: %v", err)
	}
	if table.Total != 0 || len(table.Days) != 0 {
		t.Errorf("days = %v (total %d), want none", table.Days, table.Total)
	}
}

// A name Postgres would reject must never reach the query: the whole table
// would fail rather than one heading being off by a few hours.
func TestNormalizeZoneFallsBackToUTC(t *testing.T) {
	for _, name := range []string{"", "Local", "Nowhere/Atlantis", "'; drop table assets --"} {
		if got := normalizeZone(name); got != "UTC" {
			t.Errorf("normalizeZone(%q) = %q, want UTC", name, got)
		}
	}
	if got := normalizeZone("America/New_York"); got != "America/New_York" {
		t.Errorf("normalizeZone dropped a real zone: %q", got)
	}
}

// A jump into the middle of the library has to land on exactly the row the day
// table says is there, or the tiles that arrive replace the wrong placeholders.
func TestTimelineAtLandsOnTheRowTheDayTableCounted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 20, 0, 0, 0, time.UTC)
	for i := range 30 {
		zoned(t, s, i, base.Add(-time.Duration(i)*time.Hour), minutes(0))
	}

	whole, err := s.Timeline(ctx, TimelineFilter{}, nil, 500)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}

	for _, skip := range []int{0, 1, 17, 29} {
		page, err := s.TimelineAt(ctx, TimelineFilter{}, skip, 5)
		if err != nil {
			t.Fatalf("TimelineAt(%d): %v", skip, err)
		}
		for n, it := range page.Items {
			if it.ID != whole.Items[skip+n].ID {
				t.Errorf("TimelineAt(%d) item %d = %s, want %s",
					skip, n, it.ID, whole.Items[skip+n].ID)
			}
		}
	}
}

// Past the end is an empty page, not an error: the day table a client is
// counting from can be a few uploads out of date.
func TestTimelineAtPastTheEndIsEmpty(t *testing.T) {
	s := testStore(t)
	zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))

	page, err := s.TimelineAt(context.Background(), TimelineFilter{}, 50, 10)
	if err != nil {
		t.Fatalf("TimelineAt: %v", err)
	}
	if len(page.Items) != 0 || page.NextCursor != "" {
		t.Errorf("page = %+v, want nothing", page)
	}
}

// The cursor a jumped-to page hands back has to continue from where that page
// stopped, which is what lets the grid keep paging by keyset once it has landed.
func TestTimelineAtHandsBackAUsableCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 20, 0, 0, 0, time.UTC)
	for i := range 20 {
		zoned(t, s, i, base.Add(-time.Duration(i)*time.Hour), minutes(0))
	}
	whole, err := s.Timeline(ctx, TimelineFilter{}, nil, 500)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}

	jumped, err := s.TimelineAt(ctx, TimelineFilter{}, 8, 4)
	if err != nil {
		t.Fatalf("TimelineAt: %v", err)
	}
	if jumped.NextCursor == "" {
		t.Fatal("TimelineAt returned no cursor with items left to serve")
	}
	cursor, err := DecodeCursor(jumped.NextCursor)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	next, err := s.Timeline(ctx, TimelineFilter{}, &cursor, 4)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	for n, it := range next.Items {
		if it.ID != whole.Items[12+n].ID {
			t.Errorf("continued item %d = %s, want %s", n, it.ID, whole.Items[12+n].ID)
		}
	}
}

func equalRuns(got, want [][2]any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The position has to be the index the day table counts to, or a link opens on
// the wrong photograph.
func TestTimelinePositionMatchesTheTimelineOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 5, 20, 0, 0, 0, time.UTC)
	for i := range 30 {
		zoned(t, s, i, base.Add(-time.Duration(i)*time.Hour), minutes(0))
	}

	whole, err := s.Timeline(ctx, TimelineFilter{}, nil, 500)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}

	for _, want := range []int{0, 1, 15, 29} {
		got, err := s.TimelinePosition(ctx, TimelineFilter{}, whole.Items[want].ID)
		if err != nil {
			t.Fatalf("TimelinePosition(%d): %v", want, err)
		}
		if got != want {
			t.Errorf("asset at index %d located at %d", want, got)
		}
	}
}

// Zero has to mean "the newest item" and never "not here", which is the whole
// reason the target is a subquery rather than a second lookup.
func TestTimelinePositionTellsTheNewestApartFromTheAbsent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newest := zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))
	zoned(t, s, 1, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), minutes(0))

	at, err := s.TimelinePosition(ctx, TimelineFilter{}, newest)
	if err != nil {
		t.Fatalf("TimelinePosition: %v", err)
	}
	if at != 0 {
		t.Errorf("newest item located at %d, want 0", at)
	}

	_, err = s.TimelinePosition(ctx, TimelineFilter{}, "6b3e2c1a-0000-4000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id gave %v, want ErrNotFound", err)
	}
}

// A link to a photo outside the album being browsed is the ordinary case, not
// an error, and it must not report the position that photo has in the library.
func TestTimelinePositionIsRelativeToTheCollection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	photo := zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))
	video := zoned(t, s, 1, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), minutes(0))
	if _, err := s.pool.Exec(ctx,
		`update assets set media_kind = 'video' where id = $1`, video); err != nil {
		t.Fatalf("mark video: %v", err)
	}

	at, err := s.TimelinePosition(ctx, TimelineFilter{Category: "videos"}, video)
	if err != nil {
		t.Fatalf("TimelinePosition: %v", err)
	}
	if at != 0 {
		t.Errorf("the only video located at %d in the videos timeline, want 0", at)
	}

	if _, err := s.TimelinePosition(ctx, TimelineFilter{Category: "videos"}, photo); !errors.Is(err, ErrNotFound) {
		t.Errorf("a photo located inside the videos timeline: %v", err)
	}
}

// An asset the timeline does not draw has no position in it, even though the
// row is perfectly real — a paired video is shown as its still's motion.
func TestTimelinePositionSkipsWhatTheTimelineHides(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	still := zoned(t, s, 0, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), minutes(0))
	motion := zoned(t, s, 1, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC), minutes(0))
	if _, err := s.pool.Exec(ctx,
		`update assets set live_parent_asset_id = $1 where id = $2`, still, motion); err != nil {
		t.Fatalf("pair the motion: %v", err)
	}

	if _, err := s.TimelinePosition(ctx, TimelineFilter{}, motion); !errors.Is(err, ErrNotFound) {
		t.Errorf("a paired video located in the timeline: %v", err)
	}
}
