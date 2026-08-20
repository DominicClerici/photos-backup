package vault

import (
	"crypto/ecdh"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// Index is one bucket's contents, opened.
//
// Everything the library's gallery gets from Postgres — the ordering, the day
// table, the album counts, the category covers — is computed here instead, in
// Go, over decrypted rows. Not because that is nicer, but because it is the
// only place it can happen: `order by sort_time` over a column that holds no
// capture time is not a query that can be fixed, and encrypting the metadata
// and then keeping a plaintext index to sort it by would be encrypting nothing.
//
// It is affordable because a vault is not a library. The library is a hundred
// thousand photographs and grows by itself; a vault is what somebody
// deliberately went and hid, one gesture at a time. At ten thousand items this
// is a few megabytes and a sort that takes single-digit milliseconds — and if
// an archive ever puts a hundred thousand photographs in here, the answer is a
// cached index rather than a different design.
//
// The shapes it hands back are the gallery's own — db.TimelinePage, db.DayTable
// — so the same grid, the same virtualization and the same zoom draw the vault
// as draw the library, and there is no second gallery to keep in step.
type Index struct {
	Bucket string
	// Items are the photographs, newest first. Components are not here; they
	// have already been folded into the items they belong to.
	Items []*Item
	// Albums and People are the groupings that were deliberately hidden, which
	// is not the same list as "every album some hidden photograph was in". See
	// Collections.
	Albums []db.Album
	People []string

	// all is every opened row, components included, keyed by id. The grid never
	// sees a component; the media endpoints must be able to find one, because a
	// Live Photo's motion and a caption layer are addressed by their own ids on
	// the way to disk.
	all map[string]*Item
}

// Build opens every sealed document in a bucket and assembles the index.
//
// One pass to decrypt, one to fold the components into their parents, one to
// sort. A document that will not open is skipped rather than fatal: it is one
// photograph that cannot be shown, and taking the whole page down for it would
// mean a single corrupted row hides the other four hundred.
func Build(priv *ecdh.PrivateKey, bucket string, rows []db.VaultedRow, albums []db.Album, people []string) (*Index, error) {
	if priv == nil {
		return nil, ErrLocked
	}

	opened := make([]*Item, 0, len(rows))
	for _, r := range rows {
		doc, parsed, err := openDoc(priv, r.AssetID, r.Sealed)
		if err != nil {
			continue
		}
		opened = append(opened, &Item{Bucket: r.Bucket, Doc: doc, row: parsed})
	}

	byID := make(map[string]*Item, len(opened))
	for _, it := range opened {
		byID[it.row.ID] = it
	}

	index := &Index{Bucket: bucket, Albums: albums, People: people, all: byID}
	for _, it := range opened {
		if !it.Component() {
			// An overlay is named by the picture it was drawn on rather than
			// the other way round, so this is read off the item itself.
			it.HasOverlay = it.row.OverlayAssetID != nil
			index.Items = append(index.Items, it)
			continue
		}
		// A paired video's motion state belongs to the still it belongs to.
		if it.row.LiveParentAssetID != nil {
			if parent, ok := byID[*it.row.LiveParentAssetID]; ok {
				parent.Live = it.row.LiveState
			}
		}
	}

	// The same ordering the library uses, so a vault and a gallery scroll the
	// same way: newest first, ties broken by id so the order is total.
	sort.Slice(index.Items, func(a, b int) bool {
		x, y := index.Items[a], index.Items[b]
		if !x.row.SortTime.Equal(y.row.SortTime) {
			return x.row.SortTime.After(y.row.SortTime)
		}
		return x.row.ID > y.row.ID
	})
	return index, nil
}

// Filter narrows a vault and says what order to read it in. The same fields
// db.TimelineFilter has, over the same meanings — one collection at a time, any
// combination of facets, one order — because the pill that sets them is the
// same pill, drawn over the same grid.
//
// What differs is entirely in the cost. Every one of these is a scan and a sort
// of a slice that is already in memory, so the "this one is not optimised"
// caveats the library carries about duration and album membership simply do not
// arise here: there is no index to miss.
type Filter struct {
	AlbumID  string
	Person   string
	Category string

	Sort      db.SortOrder
	Kind      string
	Favorites bool
	Unalbumed bool
}

// Empty reports the whole-bucket timeline in its natural order, which is what
// the index is already sorted into and what most requests ask for.
func (f Filter) Empty() bool {
	return f.AlbumID == "" && f.Person == "" && f.Category == "" &&
		f.Sort == db.SortNewest && f.Kind == "" && !f.Favorites && !f.Unalbumed
}

// narrowed reports whether anything has to be filtered out, as opposed to
// merely reordered.
func (f Filter) narrowed() bool {
	return f.AlbumID != "" || f.Person != "" || f.Category != "" ||
		f.Kind != "" || f.Favorites || f.Unalbumed
}

// matches is the Go half of db.categoryPred and of TimelineFilter.where. The
// two lists have to say the same thing, which is why this returns false for a
// category it does not recognise rather than passing everything: a key the
// server would have rejected in SQL must not become "the whole vault" here.
func (f Filter) matches(it *Item) bool {
	if f.AlbumID != "" {
		found := false
		for _, ref := range it.Doc.Albums {
			if ref.ID == f.AlbumID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Person != "" {
		found := false
		for _, name := range it.Doc.People {
			if name == f.Person {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Category != "" && !categoryMatch(f.Category, it) {
		return false
	}
	if f.Kind != "" && it.row.MediaKind != f.Kind {
		return false
	}
	if f.Favorites && !it.row.Favorite {
		return false
	}
	// A hidden photograph's albums are a list inside its own sealed document
	// rather than rows in a table, so "in no album" is the length of that list.
	// Which also makes it exact in a way the library's version has to work at:
	// a hidden album cannot be deleted out from under this the way a library
	// one can, because taking it apart means opening it.
	if f.Unalbumed && len(it.Doc.Albums) > 0 {
		return false
	}
	return true
}

func categoryMatch(key string, it *Item) bool {
	switch key {
	case "videos":
		return it.row.MediaKind == db.MediaVideo
	case "favorites":
		return it.row.Favorite
	case "live":
		return it.Live != "" && it.Live != db.PlaybackNone
	case "archived":
		return it.row.Archived
	case "screenshots", "panoramas", "timelapse", "cinematic", "hdr":
		want := map[string]string{
			"screenshots": "screenshot", "panoramas": "panorama",
			"timelapse": "timelapse", "cinematic": "videoCinematic", "hdr": "hdr",
		}[key]
		for _, s := range it.row.Subtypes {
			if s == want {
				return true
			}
		}
	}
	return false
}

// Select is every item a filter names, in the order it asked for.
//
// The unfiltered newest-first answer is the index itself rather than a copy of
// it — that is the request the vault's own page makes and the one worth not
// allocating for. Everything else is a fresh slice, which is what makes the
// reordering below safe: it is never sorting the index other callers hold.
func (ix *Index) Select(f Filter) []*Item {
	if f.Empty() {
		return ix.Items
	}

	out := ix.Items
	if f.narrowed() {
		out = make([]*Item, 0, len(ix.Items))
		for _, it := range ix.Items {
			if f.matches(it) {
				out = append(out, it)
			}
		}
	}
	return f.ordered(out)
}

// ordered puts a selection into the order asked for, given a slice that is
// already newest first.
//
// Oldest is a reversal rather than a sort, because the input is already
// totally ordered by exactly the key being reversed — and because reversing is
// the only way to be certain the two directions agree on where the ties go.
func (f Filter) ordered(items []*Item) []*Item {
	switch f.Sort {
	case db.SortNewest:
		return items
	case db.SortOldest:
		out := make([]*Item, len(items))
		for i, it := range items {
			out[len(items)-1-i] = it
		}
		return out
	}

	// A copy, so that sorting an unnarrowed selection cannot reorder the index
	// every other caller reads.
	out := make([]*Item, len(items))
	copy(out, items)
	longest := f.Sort == db.SortLongest
	sort.SliceStable(out, func(a, b int) bool {
		x, y := duration(out[a]), duration(out[b])
		if x == y {
			return false
		}
		// A photograph has no duration and sorts last either way, for the
		// reason the library's `nulls last` is there: these two orders are
		// offered beside a videos-only filter, but a filter is a request rather
		// than a guarantee, and "longest first" should not open with stills.
		if x < 0 || y < 0 {
			return y < 0
		}
		if longest {
			return x > y
		}
		return x < y
	})
	return out
}

// duration is an item's length in seconds, or -1 for anything that has none.
func duration(it *Item) float64 {
	if it.row.DurationSeconds == nil {
		return -1
	}
	return *it.row.DurationSeconds
}

// Page renders one page of the vault's timeline in the gallery's own shape.
//
// A cursor is honoured as well as an offset, for the same reason the library
// supports both: scrolling continues from where it was and a fling lands
// somewhere it has never been. Here the cursor is a binary search rather than a
// keyset scan, which is the same operation the index would have done.
func (ix *Index) Page(f Filter, after *db.Cursor, skip, limit int) db.TimelinePage {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	items := ix.Select(f)

	start := skip
	// The search below is a binary one, so it holds only while the slice is
	// ordered by the key it compares. In any other order the offset is the
	// answer — and it is the one the client will have sent, because a page in
	// such an order hands no cursor back. See db.TimelineFilter.keyset.
	if after != nil && f.Sort == db.SortNewest {
		start = sort.Search(len(items), func(i int) bool {
			t := items[i].row.SortTime
			if !t.Equal(after.SortTime) {
				return t.Before(after.SortTime)
			}
			return items[i].row.ID < after.ID
		})
	}
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}

	end := min(start+limit, len(items))
	page := db.TimelinePage{Items: make([]db.TimelineItem, 0, end-start)}
	for _, it := range items[start:end] {
		page.Items = append(page.Items, it.Timeline())
	}
	if end < len(items) && f.Sort == db.SortNewest {
		last := items[end-1]
		page.NextCursor = db.Cursor{SortTime: last.row.SortTime, ID: last.row.ID}.Encode()
	}
	return page
}

// Timeline renders one item as the grid draws it — deliberately the same struct
// the library's timeline sends, field for field, so the client cannot tell
// which of the two it is looking at.
func (i *Item) Timeline() db.TimelineItem {
	item := db.TimelineItem{
		ID:              i.row.ID,
		MediaKind:       i.row.MediaKind,
		TakenAt:         i.row.SortTime,
		OffsetMinutes:   i.row.ExifOffsetMinutes,
		State:           i.row.DerivedState,
		PlaybackState:   i.row.PlaybackState,
		DurationSeconds: i.row.DurationSeconds,
	}
	if i.Live != "" && i.Live != db.PlaybackNone {
		item.LiveState = i.Live
	}
	return item
}

// Locate is the id-to-position translation, for a link somebody followed into
// the vault. -1 rather than an error when the item is not in this collection,
// which is the ordinary case for a link into an album page.
func (ix *Index) Locate(f Filter, id string) int {
	for i, it := range ix.Select(f) {
		if it.row.ID == id {
			return i
		}
	}
	return -1
}

// Days counts the vault into the headings a grid draws, by exactly the rule the
// library's TimelineDays applies: the file's own UTC offset when it recorded
// one, the viewer's zone when it did not.
//
// Runs rather than a group-by, for the same reason: a date can appear twice in
// one timeline when a photograph was taken either side of a timezone hop, and a
// day table that merged them would describe a timeline the grid does not have.
func (ix *Index) Days(f Filter, zone string) db.DayTable {
	loc, name := location(zone)
	table := db.DayTable{Zone: name, Days: []db.DayRun{}}

	// An order that is not a walk through time has no days to count into, and
	// the grid gets its own size instead: one headless run, and a flat wall of
	// tiles rather than a heading above each. Exactly what the library answers,
	// for the reason it answers it. See db.DayRun.Day.
	if f.Sort == db.SortLongest || f.Sort == db.SortShortest {
		table.Total = len(ix.Select(f))
		if table.Total > 0 {
			table.Days = []db.DayRun{{Count: table.Total}}
		}
		return table
	}

	for _, it := range ix.Select(f) {
		day := dayOf(it, loc)
		if n := len(table.Days); n > 0 && table.Days[n-1].Day == day {
			table.Days[n-1].Count++
		} else {
			table.Days = append(table.Days, db.DayRun{Day: day, Count: 1})
		}
		table.Total++
	}
	return table
}

func dayOf(it *Item, loc *time.Location) string {
	t := it.row.SortTime
	if it.row.ExifOffsetMinutes != nil {
		t = t.UTC().Add(time.Duration(*it.row.ExifOffsetMinutes) * time.Minute)
		return t.Format("2006-01-02")
	}
	return t.In(loc).Format("2006-01-02")
}

// location resolves a browser's timezone name, falling back to UTC on anything
// it cannot load — the same fallback db.normalizeZone makes, for the same
// reason: a name this machine does not know is the client's problem and UTC is
// a better answer to it than a failed page.
func location(zone string) (*time.Location, string) {
	if zone == "" || zone == "Local" {
		return time.UTC, "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.UTC, "UTC"
	}
	return loc, zone
}

// Collections is the vault's own version of the collections page: the same
// three sections, over what is inside one bucket.
//
// The albums and people here are the union of two things, and the distinction
// matters. A grouping somebody deliberately hid is in this list whether or not
// anything is left in it — hiding an album and then restoring every photograph
// out of it leaves an empty hidden album, which is a true statement about what
// was decided. A grouping that merely *has* hidden photographs in it is here
// too, because the alternative is a photograph in the vault that no collection
// on the vault's own page can reach.
func (ix *Index) Collections() db.Collections {
	out := db.Collections{People: []db.Person{}, Albums: []db.Album{}, Categories: []db.Category{}}

	albums := map[string]*db.Album{}
	people := map[string]*db.Person{}

	for _, deliberate := range ix.Albums {
		albums[deliberate.ID] = &db.Album{ID: deliberate.ID, Title: deliberate.Title, Source: deliberate.Source}
	}
	for _, name := range ix.People {
		people[name] = &db.Person{Name: name}
	}

	counts := map[string]int{}
	covers := map[string]string{}

	for _, it := range ix.Items {
		for _, ref := range it.Doc.Albums {
			album, ok := albums[ref.ID]
			if !ok {
				album = &db.Album{ID: ref.ID, Title: ref.Title, Source: ref.Source}
				albums[ref.ID] = album
			}
			album.Count++
			if album.CoverID == "" {
				album.CoverID = it.row.ID
				album.NewestAt = it.row.SortTime
			}
		}
		for _, name := range it.Doc.People {
			person, ok := people[name]
			if !ok {
				person = &db.Person{Name: name}
				people[name] = person
			}
			person.Count++
			if person.CoverID == "" {
				person.CoverID = it.row.ID
			}
		}
		for _, key := range db.CategoryKeys() {
			if categoryMatch(key, it) {
				counts[key]++
				if covers[key] == "" {
					covers[key] = it.row.ID
				}
			}
		}
	}

	for _, album := range albums {
		out.Albums = append(out.Albums, *album)
	}
	sort.Slice(out.Albums, func(a, b int) bool {
		x, y := out.Albums[a], out.Albums[b]
		if !x.NewestAt.Equal(y.NewestAt) {
			return x.NewestAt.After(y.NewestAt)
		}
		return strings.ToLower(x.Title) < strings.ToLower(y.Title)
	})

	for _, person := range people {
		out.People = append(out.People, *person)
	}
	sort.Slice(out.People, func(a, b int) bool {
		if out.People[a].Count != out.People[b].Count {
			return out.People[a].Count > out.People[b].Count
		}
		return out.People[a].Name < out.People[b].Name
	})

	// Empty categories are dropped exactly as they are in the library: a slice
	// of the vault that nothing falls into is not a row worth drawing.
	for _, key := range db.CategoryKeys() {
		if counts[key] == 0 {
			continue
		}
		out.Categories = append(out.Categories, db.Category{Key: key, Count: counts[key], CoverID: covers[key]})
	}
	return out
}

// Find is the row with this id, for the media endpoints — which need the
// digest, the extension and the content type, all of which the assets row no
// longer carries. Components are findable here and nowhere else.
func (ix *Index) Find(id string) (*Item, error) {
	if it, ok := ix.all[id]; ok {
		return it, nil
	}
	return nil, fmt.Errorf("vault: no such item %s", id)
}

// LiveVideoFor is the paired video belonging to a still, which is how the
// motion endpoints reach bytes that have no id of their own in the gallery.
func (ix *Index) LiveVideoFor(stillID string) (*Item, error) {
	for _, it := range ix.all {
		if it.row.LiveParentAssetID != nil && *it.row.LiveParentAssetID == stillID {
			return it, nil
		}
	}
	return nil, fmt.Errorf("vault: %s has no motion", stillID)
}

// OverlayFor is the caption layer drawn over one picture, for the plain
// renditions the viewer's toggle asks for.
func (ix *Index) OverlayFor(id string) (*Item, error) {
	it, err := ix.Find(id)
	if err != nil {
		return nil, err
	}
	if it.row.OverlayAssetID == nil {
		return nil, fmt.Errorf("vault: %s has no overlay", id)
	}
	return ix.Find(*it.row.OverlayAssetID)
}
