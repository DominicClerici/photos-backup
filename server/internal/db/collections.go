package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A category is a slice of the library that needs no table of its own: it is a
// predicate over the asset row, named so a client can ask for it by key.
//
// The list is deliberately closed. Subtypes arrive from the phone as free-form
// strings — PhotoKit's own vocabulary, passed through untouched — so an open
// "any subtype is a category" rule would put whatever a future iOS invents into
// the UI unlabelled. Naming them here means a key the gallery cannot draw an
// icon for cannot appear.
//
// Every predicate is written against the alias `a`, and none of them may
// reference anything but the assets row: the same strings are pasted into both
// the counting query and the timeline's where clause.
type category struct {
	Key  string
	Pred string
}

var categories = []category{
	{"videos", `a.media_kind = 'video'`},
	{"favorites", `a.favorite`},
	// A still's motion is a separate asset pointing back at it, and the paired
	// video is hidden from the timeline — so "is this a Live Photo" is a
	// question about rows other than this one.
	{"live", `exists (select 1 from assets v where v.live_parent_asset_id = a.id)`},
	{"screenshots", `a.subtypes @> array['screenshot']`},
	{"panoramas", `a.subtypes @> array['panorama']`},
	{"timelapse", `a.subtypes @> array['timelapse']`},
	{"cinematic", `a.subtypes @> array['videoCinematic']`},
	{"hdr", `a.subtypes @> array['hdr']`},
	{"archived", `a.archived`},
}

func categoryPred(key string) (string, bool) {
	for _, c := range categories {
		if c.Key == key {
			return c.Pred, true
		}
	}
	return "", false
}

// Album is one album as the collections page draws it: a title, a cover, and
// how much is inside. The membership itself is never sent — that is the
// timeline's job, one page at a time.
type Album struct {
	ID string `json:"id"`
	// Source names the importer that created it, because two exports may each
	// have contributed an album of the same name and they are not the same
	// album. Shown only where that ambiguity is visible.
	Source string `json:"source"`
	Title  string `json:"title"`
	// Description is what somebody typed under the name, or what a Takeout's
	// per-directory metadata carried. Usually empty, and drawn only where there
	// is room for it — the album's own page, not the tile.
	Description string `json:"description,omitempty"`
	Count       int    `json:"count"`
	// CoverID is the newest asset in the album, or empty for an album whose
	// every member is a paired video and therefore invisible.
	CoverID string `json:"cover_id,omitempty"`
	// NewestAt is what albums are ordered by, and what the page shows under the
	// title. Zero when the album is empty.
	NewestAt time.Time `json:"newest_at,omitzero"`
}

// Person is a name a face-grouping model produced and someone confirmed, from
// an import that carried them. Not an identity, and not ours — see the
// asset_people table.
type Person struct {
	Name    string `json:"name"`
	Count   int    `json:"count"`
	CoverID string `json:"cover_id,omitempty"`
}

// Category is one named slice of the library, with a cover and a count.
type Category struct {
	Key     string `json:"key"`
	Count   int    `json:"count"`
	CoverID string `json:"cover_id,omitempty"`
}

// Collections is everything the collections page needs, in one round trip. The
// page draws all three sections at once and none of them paginate, so splitting
// this into three endpoints would only buy three times the latency.
type Collections struct {
	People     []Person   `json:"people"`
	Albums     []Album    `json:"albums"`
	Categories []Category `json:"categories"`
	// Trash is how many items are waiting in Recently Deleted. It is here
	// rather than as a category because it is not one — a category is a
	// predicate within the library and this counts what has left it — and
	// because the row that shows it sits on this page beside the categories.
	Trash int `json:"trash"`
	// Vault is how much is in each bucket, and is absent while the vault is
	// locked — not zero, absent. A locked vault says "Locked" and not "Locked,
	// 41 items": the size of the thing somebody hid is a fact about it, and the
	// whole promise here is that nothing about it is readable without the
	// password. Filled by the API, which is the layer that knows.
	Vault map[string]int `json:"vault,omitempty"`
}

// Collections reads the whole index.
//
// A failure in one section is not allowed to take the page down: an archive
// that has never been imported into has no albums and no people, and a query
// against those tables that goes wrong is still less bad than a page that will
// not load. The caller gets whatever succeeded plus the first error.
func (s *Store) Collections(ctx context.Context, peopleLimit int) (Collections, error) {
	var out Collections
	var firstErr error

	people, err := s.People(ctx, peopleLimit)
	if err != nil {
		firstErr = err
	}
	out.People = people

	albums, err := s.Albums(ctx)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	out.Albums = albums

	cats, err := s.Categories(ctx)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	out.Categories = cats

	trash, err := s.TrashCount(ctx)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	out.Trash = trash

	return out, firstErr
}

// albumSelect is shared by the list and the single-album lookup so the two
// cannot drift on what a cover or a count means.
//
// The cover is the newest *visible* member: array_agg gathers the album's own
// ids only, which is bounded by album size rather than by library size, so the
// [1] is cheap in a way a window function over the whole join would not be.
//
// A deleted album is not an empty one. Its membership is untouched and its
// photographs may still be in the library — what was thrown away is the
// grouping — so it disappears from here rather than appearing with a count of
// zero. Callers extend the where clause with `and`. See migration 0011.
//
// An album in the vault is absent for the same reason and a stronger one: it is
// on the Archive or Hidden page instead, and drawing it here would put the
// title of a hidden album on the collections page.
const albumSelect = `
	select al.id::text, al.source, al.title, al.description,
	       count(a.id),
	       coalesce((array_agg(a.id::text order by a.sort_time desc, a.id desc))[1], ''),
	       coalesce(max(a.sort_time), 'epoch'::timestamptz)
	from albums al
	left join album_assets m on m.album_id = al.id
	left join assets a on a.id = m.asset_id and ` + visibleAssets + `
	where al.deleted_at is null and al.vault = ''`

// Albums lists every album, the ones with the most recent photos first.
//
// Empty albums are kept. An import can create one from a directory whose every
// photo failed to land, and an album that silently disappears is worse than an
// album that visibly has nothing in it — the second is a thing you can go and
// investigate.
func (s *Store) Albums(ctx context.Context) ([]Album, error) {
	rows, err := s.pool.Query(ctx, albumSelect+`
		group by al.id, al.source, al.title, al.description
		order by max(a.sort_time) desc nulls last, al.title`)
	if err != nil {
		return nil, fmt.Errorf("query albums: %w", err)
	}
	defer rows.Close()

	albums := []Album{}
	for rows.Next() {
		album, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		albums = append(albums, album)
	}
	return albums, rows.Err()
}

// AlbumByID is what an album's own page reads for its heading. It exists so
// that opening one album does not cost a scan of every album.
func (s *Store) AlbumByID(ctx context.Context, id string) (Album, error) {
	rows, err := s.pool.Query(ctx, albumSelect+`
		  and al.id = $1::uuid
		group by al.id, al.source, al.title, al.description`, id)
	if err != nil {
		return Album{}, fmt.Errorf("query album %s: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Album{}, fmt.Errorf("query album %s: %w", id, err)
		}
		return Album{}, ErrNotFound
	}
	return scanAlbum(rows)
}

func scanAlbum(row scanner) (Album, error) {
	var album Album
	var newest time.Time
	if err := row.Scan(&album.ID, &album.Source, &album.Title, &album.Description,
		&album.Count, &album.CoverID, &newest); err != nil {
		return Album{}, fmt.Errorf("scan album: %w", err)
	}
	// An album with no visible members has no date, and the epoch the coalesce
	// substituted is not one — it is only there so the scan has something to
	// read into.
	if album.Count > 0 {
		album.NewestAt = newest
	}
	return album, nil
}

// People lists tagged names, the most photographed first. The limit is the
// page's, not the archive's: the row is horizontally scrollable but not
// infinite, and a Takeout can name hundreds of people who appear twice each.
func (s *Store) People(ctx context.Context, limit int) ([]Person, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		select p.name, count(*),
		       (array_agg(a.id::text order by a.sort_time desc, a.id desc))[1]
		from asset_people p
		join assets a on a.id = p.asset_id
		where `+visibleAssets+`
		  and not exists (select 1 from vault_people vp where vp.name = p.name)
		group by p.name
		order by count(*) desc, p.name
		limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query people: %w", err)
	}
	defer rows.Close()

	people := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.Name, &p.Count, &p.CoverID); err != nil {
			return nil, fmt.Errorf("scan person: %w", err)
		}
		people = append(people, p)
	}
	return people, rows.Err()
}

// Categories counts each named slice and picks it a cover.
//
// Counting is one pass over the assets with a FILTER per category, because
// every category shares the same scan. The covers are scalar subqueries beside
// it: each one walks the (sort_time desc, id desc) index and stops at the first
// row that matches, which for a category that holds a tenth of the library
// means reading about ten rows rather than all of them.
//
// Categories with nothing in them are dropped. That is what keeps a subtype
// this archive has never seen — because the export that would carry it was
// never imported — from showing up as an empty row.
func (s *Store) Categories(ctx context.Context) ([]Category, error) {
	var selects []string
	for _, c := range categories {
		selects = append(selects,
			fmt.Sprintf(`count(*) filter (where %s)`, c.Pred),
			fmt.Sprintf(`coalesce((select a.id::text from assets a where %s and %s
				order by a.sort_time desc, a.id desc limit 1), '')`, visibleAssets, c.Pred))
	}

	row := s.pool.QueryRow(ctx, `select `+strings.Join(selects, ",\n")+`
		from assets a where `+visibleAssets)

	// Scanned into a flat slice of alternating count and cover, because the
	// column list was built the same way.
	cells := make([]any, 0, len(categories)*2)
	counts := make([]int, len(categories))
	covers := make([]string, len(categories))
	for i := range categories {
		cells = append(cells, &counts[i], &covers[i])
	}
	if err := row.Scan(cells...); err != nil {
		return nil, fmt.Errorf("count categories: %w", err)
	}

	out := []Category{}
	for i, c := range categories {
		if counts[i] == 0 {
			continue
		}
		out = append(out, Category{Key: c.Key, Count: counts[i], CoverID: covers[i]})
	}
	return out, nil
}

// CategoryKeys is the closed list, in the order the collections page draws it.
//
// Exported because the vault has to ask the same question of decrypted rows in
// Go that this file asks of the library in SQL, and the two lists agreeing is
// not something to leave to whoever edits one of them. See vault.categoryMatch,
// which is the other half and is the thing that has to be kept in step.
func CategoryKeys() []string {
	keys := make([]string, len(categories))
	for i, c := range categories {
		keys[i] = c.Key
	}
	return keys
}
