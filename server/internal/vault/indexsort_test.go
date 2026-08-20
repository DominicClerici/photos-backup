package vault

import (
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// The index is built here rather than through the harness, because what is
// under test is the ordering and the facets — the part of the vault that has
// nothing to do with encryption. Everything upstream of `Items` is covered by
// the tests that do decrypt.
func indexOf(items ...*Item) *Index {
	return &Index{Bucket: db.VaultHidden, Items: items, all: map[string]*Item{}}
}

func hidden(id string, at time.Time, opts ...func(*Item)) *Item {
	it := &Item{Bucket: db.VaultHidden, row: row{
		ID: id, MediaKind: db.MediaImage, SortTime: at, DerivedState: db.DerivedReady,
	}}
	for _, opt := range opts {
		opt(it)
	}
	return it
}

func clip(seconds float64) func(*Item) {
	return func(it *Item) {
		it.row.MediaKind = db.MediaVideo
		it.row.DurationSeconds = &seconds
	}
}

func starred(it *Item) { it.row.Favorite = true }

func filed(it *Item) {
	it.Doc.Albums = []AlbumRef{{ID: "an-album", Title: "Private"}}
}

func picked(t *testing.T, ix *Index, f Filter) []string {
	t.Helper()
	out := []string{}
	for _, it := range ix.Select(f) {
		out = append(out, it.ID())
	}
	return out
}

func equal(a, b []string) bool {
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

// The vault answers the same questions the library does, over the same grid, so
// the orders have to mean the same things — including where a photograph with
// no duration lands under an order about duration.
func TestVaultReadsInEveryOrderTheLibraryDoes(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	brief := hidden("a", base, clip(3))
	epic := hidden("b", base.Add(-time.Hour), clip(900))
	still := hidden("c", base.Add(-2*time.Hour))
	ix := indexOf(brief, epic, still)

	for _, tc := range []struct {
		sort db.SortOrder
		want []string
	}{
		{db.SortNewest, []string{"a", "b", "c"}},
		{db.SortOldest, []string{"c", "b", "a"}},
		{db.SortLongest, []string{"b", "a", "c"}},
		{db.SortShortest, []string{"a", "b", "c"}},
	} {
		if got := picked(t, ix, Filter{Sort: tc.sort}); !equal(got, tc.want) {
			t.Errorf("%q = %v, want %v", tc.sort, got, tc.want)
		}
	}
}

// Reordering must never reorder the index itself: it is held for the life of an
// unlocked vault and read by every other request against it.
func TestReorderingLeavesTheIndexAlone(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	ix := indexOf(hidden("a", base, clip(3)), hidden("b", base.Add(-time.Hour), clip(900)))

	if got := picked(t, ix, Filter{Sort: db.SortLongest}); !equal(got, []string{"b", "a"}) {
		t.Fatalf("longest = %v, want [b a]", got)
	}
	if got := picked(t, ix, Filter{}); !equal(got, []string{"a", "b"}) {
		t.Errorf("the index itself is now %v, want it untouched as [a b]", got)
	}
}

func TestVaultFacetsCombine(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	ix := indexOf(
		hidden("a", base, clip(3), starred),
		hidden("b", base.Add(-time.Hour), clip(900), filed),
		hidden("c", base.Add(-2*time.Hour), starred, filed),
	)

	for _, tc := range []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"videos", Filter{Kind: db.MediaVideo}, []string{"a", "b"}},
		{"photos", Filter{Kind: db.MediaImage}, []string{"c"}},
		{"favorites", Filter{Favorites: true}, []string{"a", "c"}},
		{"loose", Filter{Unalbumed: true}, []string{"a"}},
		{"favorite videos", Filter{Favorites: true, Kind: db.MediaVideo}, []string{"a"}},
		{"oldest favorites", Filter{Favorites: true, Sort: db.SortOldest}, []string{"c", "a"}},
	} {
		if got := picked(t, ix, tc.filter); !equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A timeline ordered by length has no days in it, and the grid is told so in
// exactly the shape the library uses: one run, carrying the whole count, with
// no date on it.
func TestVaultDurationOrdersHaveNoDays(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	ix := indexOf(hidden("a", base, clip(3)), hidden("b", base.Add(-40*time.Hour), clip(900)))

	table := ix.Days(Filter{Sort: db.SortLongest}, "UTC")
	if table.Total != 2 {
		t.Fatalf("Total = %d, want 2", table.Total)
	}
	if len(table.Days) != 1 || table.Days[0].Day != "" || table.Days[0].Count != 2 {
		t.Errorf("days = %v, want one headless run of 2", table.Days)
	}

	// And the day table for an order that does have days follows that order.
	dated := ix.Days(Filter{Sort: db.SortOldest}, "UTC")
	if len(dated.Days) != 2 || dated.Days[0].Day != "2026-08-03" {
		t.Errorf("oldest days = %v, want the older heading first", dated.Days)
	}
}

// A page in an order that cannot express a cursor must not hand one out: the
// client would page from a position meaning something else entirely.
func TestVaultHandsOutCursorsOnlyForTheOrderThatHasOne(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	ix := indexOf(hidden("a", base, clip(3)), hidden("b", base.Add(-time.Hour), clip(900)))

	if page := ix.Page(Filter{}, nil, 0, 1); page.NextCursor == "" {
		t.Error("newest handed out no cursor, want one")
	}
	if page := ix.Page(Filter{Sort: db.SortLongest}, nil, 0, 1); page.NextCursor != "" {
		t.Errorf("longest handed out %q, want no cursor", page.NextCursor)
	}
}
