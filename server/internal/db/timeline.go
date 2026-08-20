package db

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TimelineItem is deliberately the smallest thing the grid can draw: an id to
// fetch a thumbnail with, a time to group under a heading, and enough state to
// know whether the thumbnail exists yet. Around 80 bytes on the wire, so a
// 200-item page is ~16KB.
type TimelineItem struct {
	ID        string    `json:"id"`
	MediaKind string    `json:"kind"`
	TakenAt   time.Time `json:"taken_at"`
	// OffsetMinutes is the file's own UTC offset, sent so the grid can file a
	// photo under the day it was taken rather than the day it falls on in
	// whatever timezone the browser happens to be in. Null means the file
	// recorded no zone, and the client falls back to local time.
	OffsetMinutes   *int     `json:"offset_minutes,omitempty"`
	State           string   `json:"state"`
	PlaybackState   string   `json:"playback_state,omitempty"`
	DurationSeconds *float64 `json:"duration,omitempty"`
	// LiveState is the state of this still's paired video, for the half of an
	// iPhone library that is Live Photos. Omitted when there is no motion, so
	// the field's presence is the whole answer the grid needs.
	LiveState string `json:"live,omitempty"`
}

// notComponent excludes the assets that are parts of other assets rather than
// items in their own right — the paired videos, shown as their still's motion
// rather than as items of their own, and the Snapchat overlays, composed into
// the photo they were drawn on rather than shown as transparent PNGs.
//
// A phone's declaration is enough on its own: it is only ever made about a
// video whose still is already queued behind it, so hiding does not have to
// wait for the pairing to complete.
//
// An identifier read off an imported file is not enough on its own, which is
// why the resolution is checked too rather than instead. An export can hold a
// paired video whose still was deleted years ago — a third of the sample export
// is exactly that — and those are ordinary videos as far as anyone looking at
// this archive is concerned. Hiding one would archive it into invisibility;
// showing it costs a duplicate-looking tile for as long as it takes the still
// to turn up, and it disappears by itself when one does.
//
// An overlay is hidden unconditionally, which is the opposite call and rests on
// the opposite fact: it is not a photograph that might have to stand alone, it
// is one layer of somebody's handwriting over a photograph that is in this
// archive too. Snapchat never showed it by itself and neither should this.
//
// Written against the alias `a`, which every query that pastes it in has to
// provide — including the UPDATEs and DELETEs in trash.go, which alias their
// target for exactly this reason. Unqualified was enough until albums grew a
// deleted_at of its own and the album join became ambiguous.
//
// It must stay identical to the leading terms of assets_timeline_visible_idx
// and assets_trash_idx, or the timeline stops being an index scan. See
// migrations 0009 and 0011.
const notComponent = `a.live_parent_local_id = '' and a.live_parent_asset_id is null and not a.is_overlay`

// visibleAssets is the library: everything the gallery draws. A component of
// another item is not one, neither is anything in the trash, and neither is
// anything in the vault.
//
// The trash is the same predicate with the deleted term flipped — see
// TimelineFilter.scope. That is the whole of what makes Recently Deleted a
// scope rather than a collection: one timeline, one ordering, one day table,
// read over the other half of a boolean.
//
// The vault is not the same trick and could not be. Its term appears on both of
// these predicates rather than being flipped between them, because what is in
// the vault is not a third view of the same rows — every column those rows
// would be drawn from has been encrypted, and there is nothing here for SQL to
// order, group or count. See db.vaultedAssets and internal/vault.Index.
const visibleAssets = notComponent + ` and a.deleted_at is null and a.vault = ''`

// trashedAssets is the other half: deleted, not yet purged, and still whole —
// a still that went to the trash took its motion with it, and both come back
// together.
const trashedAssets = notComponent + ` and a.deleted_at is not null and a.vault = ''`

// liveJoin attaches a still's motion state to whichever relation the caller
// named. Lateral with a limit rather than a plain join: the same still can be
// reached by two devices' copies of the same paired video, and a plain join
// would draw that photo twice.
//
// The alias is a parameter because the timeline hangs this off a subquery it
// has already cut to one page, while the state poll hangs it off the assets
// table directly.
func liveJoin(alias string) string {
	return `
	left join lateral (
		select v.live_state from assets v
		where v.live_parent_asset_id = ` + alias + `.id
		order by v.uploaded_at, v.id limit 1
	) live on true`
}

// TimelinePage is one page of the timeline. NextCursor is empty at the end.
type TimelinePage struct {
	Items      []TimelineItem `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// Cursor is the keyset position between pages: the sort key of the last item
// returned, plus its id to break ties between assets sharing a timestamp.
type Cursor struct {
	SortTime time.Time
	ID       string
}

var ErrBadCursor = errors.New("db: malformed timeline cursor")

// Encode renders a cursor as an opaque token. Opaque because it is a position
// in a result set, not an API: making it look unparseable keeps clients from
// building on its shape.
func (c Cursor) Encode() string {
	raw := c.SortTime.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func DecodeCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return Cursor{}, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{SortTime: t, ID: id}, nil
}

// SortOrder is the order a timeline is read in.
//
// Two of the four are the same walk through time read in opposite directions,
// and the index serves both — a b-tree scanned backwards is still an index
// scan. The other two order by a column with no index at all, which is a
// deliberate trade: see order and keyset.
type SortOrder string

const (
	// SortNewest is the zero value, so a filter that says nothing about order
	// asks for the timeline every other comment in this file describes.
	SortNewest   SortOrder = ""
	SortOldest   SortOrder = "oldest"
	SortLongest  SortOrder = "longest"
	SortShortest SortOrder = "shortest"
)

// ErrUnknownSort names an order that is not one of the four.
var ErrUnknownSort = errors.New("db: unknown sort order")

// ParseSort reads the wire spelling of an order.
//
// "newest" is accepted as well as absent: a client that names the default
// explicitly is not making a mistake, and refusing it would make the one order
// everything defaults to the only one that cannot be asked for by name.
func ParseSort(name string) (SortOrder, error) {
	switch SortOrder(name) {
	case SortNewest, SortOldest, SortLongest, SortShortest:
		return SortOrder(name), nil
	}
	if name == "newest" {
		return SortNewest, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownSort, name)
}

// byDuration reports the two orders that are not a walk through time.
func (s SortOrder) byDuration() bool {
	return s == SortLongest || s == SortShortest
}

// TimelineFilter narrows the timeline and says what order to read it in.
//
// The zero value is the whole library, newest first, which is what the gallery
// itself asks for — so a narrowed timeline is the same query, the same cursor,
// and the same page shape as the unnarrowed one rather than a second endpoint
// that would have to be kept in step with it.
//
// The fields fall into two groups that behave differently. A collection —
// album, person, category — is a *place*, and at most one may be named: setting
// two asks for their intersection, which is a coherent answer to a question
// nothing poses. The facets below them are not places but adjectives, and they
// combine freely with a collection and with each other, because "the videos in
// this album that are not favourites" is a question somebody does pose.
type TimelineFilter struct {
	// AlbumID is an album's uuid. Membership rather than the album's own row,
	// so an id that names no album is an empty timeline, not an error.
	AlbumID string
	// Person is an exact tagged name, as stored — these come from an import,
	// not from typing, so there is nothing to normalise.
	Person string
	// Category is one of the keys in collections.go. An unknown key is
	// rejected rather than ignored: silently serving the whole library for a
	// misspelt category is the kind of bug that looks like it works.
	Category string

	// Trash reads the deleted half of the archive instead of the live one.
	//
	// Not a category, though it would fit the shape: every category is a
	// predicate *within* the library, and this one replaces the rule that says
	// what the library is. Keeping it a separate field is what makes it
	// impossible to write a filter that returns the two mixed together.
	Trash bool

	// Sort is the order, and the zero value is the one everything is indexed
	// for. See SortOrder.
	Sort SortOrder

	// Kind narrows to photographs or to videos: MediaImage or MediaVideo, and
	// empty for both. It is the one facet the timeline index can help with, and
	// the one the gallery spends most of its time under.
	Kind string
	// Favorites keeps what somebody starred, in whatever app exported it.
	Favorites bool
	// Unalbumed keeps what is in no album — the pile left over after the
	// organising, which is the whole reason to ask for it.
	Unalbumed bool

	// The three the search box added. They are facets like the ones above —
	// adjectives, combining freely — rather than collections, which is why they
	// sit here and not beside AlbumID.

	// After and Before bound sort_time, and both are inclusive of the whole
	// civil day they name: "June 2025" is 2025-06-01 to 2025-06-30 and holds
	// everything taken on the 30th. Inclusive because both ends are shown to a
	// person as a removable chip, and an exclusive end would put a month in the
	// chip that was not in the question. Compared in UTC, which can file a
	// photograph taken at 11pm on the last day under the next one — the ranges
	// the parser produces are generous enough to absorb that, deliberately.
	//
	// The comparison lands on assets_timeline_visible_idx's leading column, so
	// a date range is a narrowed index scan rather than a filter over one.
	After  *time.Time
	Before *time.Time

	// Place is where, at whichever of the three levels was named. Exactly one
	// of its fields is set: which one decides which column is compared, and
	// matching "California" against place_city would find nothing while looking
	// like it worked.
	Place *Place

	// Tags keeps assets carrying any of these, resolved through
	// tags.canonical_id — so a filter for "dog" finds what the model called a
	// puppy, and a merge takes effect here without anything being rewritten.
	//
	// Any rather than all. The vocabulary is free-form and a model that wrote
	// "dog" about one photograph and "puppy" about the next is the ordinary
	// case rather than the pathological one; an intersection over two words
	// from that vocabulary is usually empty. Precision comes from the ranking,
	// not from this.
	Tags []string

	// People is the conjunction Person is not. Person names a collection — one
	// person's page, at most one at a time — and this is the search's version:
	// "phoenix and dominic" means both are in the photograph, so every name
	// here is an AND.
	People []string
}

// ErrUnknownKind names a media kind that is neither of the two.
var ErrUnknownKind = errors.New("db: unknown media kind")

// order renders the ordering as a SQL fragment against a given alias.
//
// The alias is a parameter because the same order is written twice per page:
// once inside the subquery that cuts it, and once outside to restore it after
// the lateral join. Both have to say the same thing or a page comes back
// shuffled.
//
// The duration orders carry `nulls last`, which is not the default for `desc`.
// A null duration is a photograph, and the gallery only offers these two orders
// alongside a videos-only filter — but a filter is a request, not a guarantee,
// and "longest first" should not open with every still in the archive.
func (f TimelineFilter) order(alias string) string {
	switch f.Sort {
	case SortOldest:
		return alias + ".sort_time asc, " + alias + ".id asc"
	case SortLongest:
		return alias + ".duration_seconds desc nulls last, " + alias + ".id desc"
	case SortShortest:
		return alias + ".duration_seconds asc nulls last, " + alias + ".id asc"
	default:
		return alias + ".sort_time desc, " + alias + ".id desc"
	}
}

// keyset reports whether a page in this order can say where the next one
// begins, which is what decides between a cursor and a row offset.
//
// Only the two orders over `sort_time` can. A cursor is the sort key of the
// last row plus its id, and it is worth having because the timeline's index
// holds exactly that pair — so continuing from one is a seek rather than a
// count. Duration has no such index and would need a nullable key in the
// cursor to boot, so those two orders page by offset instead: every page pays
// for a sort of the whole filtered set, which is the cost of an order somebody
// reaches for once and scrolls a few screens of. See TimelineAt.
func (f TimelineFilter) keyset() bool {
	return !f.Sort.byDuration()
}

// beyond is the comparison a cursor makes: rows that sort after the one the
// last page ended on, in whichever direction this order runs.
func (f TimelineFilter) beyond() string {
	if f.Sort == SortOldest {
		return ">"
	}
	return "<"
}

// ahead is the same comparison read the other way: rows this one has already
// passed, which counted is a position. See TimelinePosition.
func (f TimelineFilter) ahead() string {
	if f.Sort == SortOldest {
		return "<"
	}
	return ">"
}

// scope is the rule that says which assets this filter can see at all, before
// any narrowing. See visibleAssets.
func (f TimelineFilter) scope() string {
	if f.Trash {
		return trashedAssets
	}
	return visibleAssets
}

// ErrUnknownCategory names a category key that no predicate matches.
var ErrUnknownCategory = errors.New("db: unknown category")

// ErrUnknownPlace names a place filter that carries no level at all, which is a
// caller bug rather than a request that found nothing.
var ErrUnknownPlace = errors.New("db: place names no city, state or country")

// where renders the filter as a SQL fragment plus the arguments it needs,
// numbered from next.
func (f TimelineFilter) where(next int) (string, []any, error) {
	var clauses []string
	var args []any

	if f.AlbumID != "" {
		clauses = append(clauses, fmt.Sprintf(
			`exists (select 1 from album_assets m
			         where m.asset_id = a.id and m.album_id = $%d::uuid)`, next))
		args = append(args, f.AlbumID)
		next++
	}
	if f.Person != "" {
		clauses = append(clauses, fmt.Sprintf(
			`exists (select 1 from asset_people p
			         where p.asset_id = a.id and p.name = $%d)`, next))
		args = append(args, f.Person)
		next++
	}
	if f.Category != "" {
		pred, ok := categoryPred(f.Category)
		if !ok {
			return "", nil, fmt.Errorf("%w: %q", ErrUnknownCategory, f.Category)
		}
		// No argument: the predicate is one of ours, from a closed list, and
		// never carries anything the request supplied.
		clauses = append(clauses, pred)
	}

	for _, name := range f.People {
		clauses = append(clauses, fmt.Sprintf(
			`exists (select 1 from asset_people p
			         where p.asset_id = a.id and p.name = $%d)`, next))
		args = append(args, name)
		next++
	}

	if f.After != nil {
		clauses = append(clauses, fmt.Sprintf(`a.sort_time >= $%d`, next))
		args = append(args, f.After.UTC())
		next++
	}
	if f.Before != nil {
		// The day named is included whole, which is what the chip says. A
		// half-open comparison against the following midnight rather than
		// `<= 23:59:59`, so nothing taken in the last second of the day falls
		// through a gap between two representations of "the end".
		clauses = append(clauses, fmt.Sprintf(`a.sort_time < $%d`, next))
		args = append(args, f.Before.UTC().AddDate(0, 0, 1))
		next++
	}

	if f.Place != nil {
		column, value := f.Place.column()
		if column == "" {
			return "", nil, fmt.Errorf("%w: a place naming nothing", ErrUnknownPlace)
		}
		clauses = append(clauses, fmt.Sprintf(`a.%s = $%d`, column, next))
		args = append(args, value)
		next++
	}

	if len(f.Tags) > 0 {
		// Resolved through the merge on the way out, which is what
		// canonical_id is for: the row records the word the model wrote, and
		// this is where "puppy" becomes "dog" — everywhere at once, and
		// reversibly.
		clauses = append(clauses, fmt.Sprintf(`exists (
			select 1 from asset_tags at
			join tags tag on tag.id = at.tag_id
			left join tags canonical on canonical.id = tag.canonical_id
			where at.asset_id = a.id
			  and coalesce(canonical.name, tag.name) = any($%d::text[]))`, next))
		args = append(args, f.Tags)
		next++
	}

	if f.Kind != "" {
		if f.Kind != MediaImage && f.Kind != MediaVideo {
			return "", nil, fmt.Errorf("%w: %q", ErrUnknownKind, f.Kind)
		}
		clauses = append(clauses, fmt.Sprintf(`a.media_kind = $%d`, next))
		args = append(args, f.Kind)
		next++
	}
	if f.Favorites {
		clauses = append(clauses, `a.favorite`)
	}
	if f.Unalbumed {
		// A deleted album is not an album any more, and neither is a hidden
		// one. Their membership rows survive both — that is what makes the
		// undo work — so joining is what keeps a photograph whose only album
		// went to the trash from being invisible in the one filter that exists
		// to find it.
		clauses = append(clauses, `not exists (
			select 1 from album_assets m
			join albums al on al.id = m.album_id
			where m.asset_id = a.id and al.deleted_at is null and al.vault = '')`)
	}

	if len(clauses) == 0 {
		return "", nil, nil
	}
	return " and " + strings.Join(clauses, " and "), args, nil
}

// Timeline returns one page of assets, newest first. An empty cursor starts at
// the beginning.
//
// Pagination is keyset rather than OFFSET, so page 400 costs exactly what page
// 1 costs. The row comparison `(sort_time, id) < (...)` maps directly onto the
// (sort_time desc, id desc) index, which is what keeps it a plain index scan.
//
// Assets whose derivatives are still pending or have failed are included. The
// alternative — hiding them until ready — means most of the library is silently
// absent during a backfill, and a permanently failed job produces a photo that
// is archived but unreachable.
func (s *Store) Timeline(ctx context.Context, filter TimelineFilter, after *Cursor, limit int) (TimelinePage, error) {
	return s.timeline(ctx, filter, after, 0, limit)
}

// TimelineAt returns the page starting at a row offset into the same ordering,
// which is how the gallery fills a stretch of grid it has scrolled straight to
// rather than paged down into.
//
// This is the OFFSET that Timeline exists to avoid, and it is the right call
// here for the same reason it is the wrong one there. Sequential paging visits
// every row anyway, so making page 400 re-count the 80,000 rows behind it is
// pure waste; a jump has visited none of them, and counting is the cheapest way
// to find out where to start. The alternative — an anchor cursor per day,
// shipped with the day table — costs eighty bytes a day on every page load to
// save a scan that happens once per fling.
//
// The offset means something to a client only because TimelineDays counts in
// the same units: its run lengths sum to a position in exactly this ordering.
// Nothing may treat it as stable — an import that rewrites one photo's date
// moves every row after it — which is why the day table and the pages drawn
// from it are fetched together and checked against each other.
func (s *Store) TimelineAt(ctx context.Context, filter TimelineFilter, skip, limit int) (TimelinePage, error) {
	return s.timeline(ctx, filter, nil, skip, limit)
}

func (s *Store) timeline(ctx context.Context, filter TimelineFilter, after *Cursor, skip, limit int) (TimelinePage, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if skip < 0 {
		skip = 0
	}

	var (
		cursorTime any
		cursorID   any
	)
	// A cursor is a position in an ordering that has one; an order this page
	// cannot hand a cursor out for is one it must not accept a stale cursor
	// into either. Ignored rather than refused, because the offset the request
	// also carries is a perfectly good answer to where the page starts.
	if after != nil && filter.keyset() {
		cursorTime = after.SortTime
		cursorID = after.ID
	}

	// $1 and $2 are the cursor, $3 the offset and $4 the row count; the
	// filter's own arguments start after them.
	narrow, args, err := filter.where(5)
	if err != nil {
		return TimelinePage{}, err
	}
	args = append([]any{cursorTime, cursorID, skip, limit + 1}, args...)

	// The page is cut before the motion lookup rather than after it, so a jump
	// deep into the library runs the lateral once per row it keeps instead of
	// once per row it counted past.
	//
	// One extra row tells us whether another page exists without a second query.
	rows, err := s.pool.Query(ctx, `
		select p.id::text, p.media_kind, p.sort_time, p.exif_offset_minutes,
		       p.derived_state, p.playback_state, p.duration_seconds,
		       coalesce(live.live_state, 'none')
		from (
			select a.id, a.media_kind, a.sort_time, a.exif_offset_minutes,
			       a.derived_state, a.playback_state, a.duration_seconds
			from assets a
			where `+filter.scope()+`
			  and ($1::timestamptz is null
			       or (a.sort_time, a.id) `+filter.beyond()+` ($1::timestamptz, $2::uuid))`+narrow+`
			order by `+filter.order("a")+`
			offset $3 limit $4
		) p`+liveJoin("p")+`
		order by `+filter.order("p"), args...)
	if err != nil {
		return TimelinePage{}, fmt.Errorf("query timeline: %w", err)
	}
	defer rows.Close()

	var page TimelinePage
	var last Cursor
	for rows.Next() {
		var it TimelineItem
		var sortTime time.Time
		if err := rows.Scan(&it.ID, &it.MediaKind, &sortTime, &it.OffsetMinutes,
			&it.State, &it.PlaybackState, &it.DurationSeconds, &it.LiveState); err != nil {
			return TimelinePage{}, fmt.Errorf("scan timeline item: %w", err)
		}
		it.TakenAt = sortTime
		it.hideEmptyLiveState()
		page.Items = append(page.Items, it)
		last = Cursor{SortTime: sortTime, ID: it.ID}
	}
	if err := rows.Err(); err != nil {
		return TimelinePage{}, fmt.Errorf("read timeline: %w", err)
	}

	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last = Cursor{SortTime: page.Items[limit-1].TakenAt, ID: page.Items[limit-1].ID}
		// Silence is what tells a client to keep using offsets. An order with
		// no keyset has no position to hand over, and a cursor built from a
		// sort key this ordering does not use would send the next page to a
		// different part of the archive entirely.
		if filter.keyset() {
			page.NextCursor = last.Encode()
		}
	}
	return page, nil
}

// TimelinePosition is where one asset sits in a timeline: the index a client
// would find it at, counting from the newest.
//
// It answers the one question the day table cannot. Everything else about this
// timeline is addressed by position — the grid draws positions, TimelineAt
// fetches them — but a link someone pasted carries an id, and an id has no
// position until something counts. Walking the pages until it turns up is what
// the gallery used to do, and it costs a request per two hundred photographs to
// answer a question one count already knows.
//
// The count is over the ordering itself: how many visible items sort ahead of
// this one. Which makes it exact rather than approximate, and makes it agree
// with TimelineAt by construction — they are the same comparison read in
// opposite directions.
//
// ErrNotFound covers both "no such asset" and "not in this timeline". They are
// the same answer to the caller: there is no position here to go to. An album
// page handed a link to a photo outside that album is the ordinary case, and it
// is not an error.
func (s *Store) TimelinePosition(ctx context.Context, filter TimelineFilter, id string) (int, error) {
	// An order with no keyset has no "sorts ahead of" to count either — the
	// comparison below is over the sort key, and duration's is nullable. Those
	// two are ranked instead. See rank.
	if !filter.keyset() {
		return s.rank(ctx, filter, id)
	}

	// $1 is the asset; the filter's own arguments start after it. The fragment
	// is pasted twice and both copies alias the table `a`, so they share their
	// placeholders rather than needing a second set.
	narrow, args, err := filter.where(2)
	if err != nil {
		return 0, err
	}
	args = append([]any{id}, args...)

	// The target is a subquery rather than a separate round trip so that an
	// asset which is not in this timeline produces no row at all, which is what
	// tells "it is the newest item" apart from "it is not here" — both of which
	// would otherwise be a count of zero.
	row := s.pool.QueryRow(ctx, `
		select (
			select count(*)::int
			from assets a
			where `+filter.scope()+`
			  and (a.sort_time, a.id) `+filter.ahead()+` (t.sort_time, t.id)`+narrow+`
		)
		from (
			select a.sort_time, a.id
			from assets a
			where a.id = $1::uuid and `+filter.scope()+narrow+`
		) t`, args...)

	var index int
	if err := row.Scan(&index); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("locate asset in timeline: %w", err)
	}
	return index, nil
}

// rank is TimelinePosition for the orders that cannot count what sorts ahead.
//
// It numbers the whole filtered timeline and reads off the row it was asked
// about, which is a sort of everything to answer a question about one thing.
// That is the same bargain the duration orders make everywhere else — no index,
// so every page pays for the sort — and it is worth making twice rather than
// keeping a nullable duration in the cursor and a second comparison beside the
// one above.
func (s *Store) rank(ctx context.Context, filter TimelineFilter, id string) (int, error) {
	// $1 is the asset; the filter's own arguments start after it.
	narrow, args, err := filter.where(2)
	if err != nil {
		return 0, err
	}
	args = append([]any{id}, args...)

	// Numbered from zero, because that is what the day table's run lengths sum
	// to and what TimelineAt skips in.
	row := s.pool.QueryRow(ctx, `
		select o.rn from (
			select a.id, row_number() over (order by `+filter.order("a")+`) - 1 as rn
			from assets a
			where `+filter.scope()+narrow+`
		) o
		where o.id = $1::uuid`, args...)

	var index int
	if err := row.Scan(&index); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("rank asset in timeline: %w", err)
	}
	return index, nil
}

// DayRun is one heading in the timeline and the number of tiles that hang under
// it: everything the grid needs to know a day's size without holding a single
// one of its photos.
//
// A run rather than a date, because a date is not unique. Items are ordered by
// instant and filed under their own local day, so a photo taken either side of
// a timezone hop can put a date on both sides of another one and split it into
// two runs. Sending runs is what makes the day table a description of the
// timeline's shape rather than a summary that disagrees with it.
type DayRun struct {
	// Day is the local calendar day, YYYY-MM-DD. Rendered by the client, which
	// is the only party that knows what language to render it in.
	//
	// Empty means this run has no date and draws no heading, which is what a
	// timeline ordered by something other than time is: the days are still
	// there, scattered through it in an order that has nothing to do with the
	// calendar, and a heading per tile is not a description of that shape but a
	// ruin of it. See TimelineDays.
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// DayTable is the shape of a whole filtered timeline, sent before any of it.
//
// It is what lets the gallery draw a scrollbar that is already the right
// height, headings that are already in the right places, and placeholder tiles
// under them that are already the right size — so scrolling never hits a wall
// and nothing shifts under the pointer as pages land. Positions in it are the
// same positions TimelineAt counts in.
//
// A day is around thirty bytes here, so a fifteen-year library costs on the
// order of a hundred kilobytes uncompressed and a fraction of that on the wire.
// That is a page-load cost paid once per collection opened, against a query the
// server can answer from one index scan.
type DayTable struct {
	// Zone is the timezone the days were bucketed in, echoed back because it
	// may not be the one that was asked for.
	Zone string `json:"tz"`
	// Total is the sum of the run lengths: how many items the filtered timeline
	// holds, known before any of them are fetched.
	Total int      `json:"total"`
	Days  []DayRun `json:"days"`
}

// utcZone is what an absent or unrecognised timezone falls back to.
const utcZone = "UTC"

// normalizeZone keeps a name Postgres would reject out of the query.
//
// The name arrives from a browser and goes straight into `at time zone`, where
// an unknown one is an error that would take the whole day table down rather
// than one heading. Go and Postgres read the same tzdata, so a name Go can load
// is a name Postgres knows; one it cannot is the client's problem, and UTC is a
// better answer to it than a failed page.
//
// "Local" is excluded even though Go accepts it: it means the *server's* zone
// there, which is not a thing the browser could have meant, and Postgres has no
// such name anyway.
func normalizeZone(name string) string {
	if name == "" || name == "Local" {
		return utcZone
	}
	if _, err := time.LoadLocation(name); err != nil {
		return utcZone
	}
	return name
}

// TimelineDays counts the timeline into the days it draws as headings.
//
// The bucketing rule is the same one the grid used to apply per item: the
// file's own UTC offset when it recorded one, so a photo taken at 23:50 in
// Vermont files under that day rather than the next one for a viewer in Berlin,
// and the viewer's zone when it did not. Which is why the zone has to come from
// the client — it is the fallback, not the rule.
//
// The runs are found with the gaps-and-islands trick rather than a group-by,
// because a group-by would merge a date that appears twice into one heading and
// hand back a shape the timeline does not have. It costs one ordered pass over
// the filtered rows, which the timeline index already provides in order.
func (s *Store) TimelineDays(ctx context.Context, filter TimelineFilter, zone string) (DayTable, error) {
	table := DayTable{Zone: normalizeZone(zone), Days: []DayRun{}}

	// An order that is not a walk through time has no days to count into. What
	// the grid needs from this is its own size, so that is all it gets: one
	// headless run, and a flat wall of tiles rather than a heading above each.
	if filter.Sort.byDuration() {
		return s.timelineSize(ctx, filter, table)
	}

	// $1 is the timezone; the filter's own arguments start after it.
	narrow, args, err := filter.where(2)
	if err != nil {
		return DayTable{}, err
	}
	args = append([]any{table.Zone}, args...)

	rows, err := s.pool.Query(ctx, `
		select to_char(day, 'YYYY-MM-DD'), count(*)::int
		from (
			select day, rn, rn - row_number() over (partition by day order by rn) as run
			from (
				select date(case
				           when a.exif_offset_minutes is null
				               then a.sort_time at time zone $1
				           else (a.sort_time at time zone 'UTC')
				                + make_interval(mins => a.exif_offset_minutes)
				       end) as day,
				       row_number() over (order by `+filter.order("a")+`) as rn
				from assets a
				where `+filter.scope()+narrow+`
			) dated
		) runs
		group by day, run
		order by min(rn)`, args...)
	if err != nil {
		return DayTable{}, fmt.Errorf("query timeline days: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var run DayRun
		if err := rows.Scan(&run.Day, &run.Count); err != nil {
			return DayTable{}, fmt.Errorf("scan timeline day: %w", err)
		}
		table.Days = append(table.Days, run)
		table.Total += run.Count
	}
	if err := rows.Err(); err != nil {
		return DayTable{}, fmt.Errorf("read timeline days: %w", err)
	}
	return table, nil
}

// timelineSize is the day table for a timeline that has no days: how many
// items, said in the one shape the grid knows how to be laid out from.
//
// One run rather than none, because a client that received an empty table would
// read it as an empty collection. The run carries no date, which is what tells
// the grid to draw no headings — see DayRun.Day.
func (s *Store) timelineSize(ctx context.Context, filter TimelineFilter, table DayTable) (DayTable, error) {
	narrow, args, err := filter.where(1)
	if err != nil {
		return DayTable{}, err
	}

	row := s.pool.QueryRow(ctx, `
		select count(*)::int from assets a
		where `+filter.scope()+narrow, args...)
	if err := row.Scan(&table.Total); err != nil {
		return DayTable{}, fmt.Errorf("count timeline: %w", err)
	}
	if table.Total > 0 {
		table.Days = []DayRun{{Count: table.Total}}
	}
	return table, nil
}

// TimelineStates returns the current derivative state of specific assets. The
// gallery polls this for the pending tiles it has on screen, which is far
// cheaper than re-fetching whole pages while a backfill runs.
//
// Vaulted assets are not answered for, and that is not an oversight in the
// direction of secrecy — the row genuinely has nothing left to report. Its
// capture time and its offset were encrypted away, so answering would patch a
// tile's date to its upload time and shuffle the grid; and its queued work was
// dropped when it was hidden, so the state it is in is the state it will stay
// in until it comes back out. Silence is the accurate answer to a poll about
// something that cannot change.
func (s *Store) TimelineStates(ctx context.Context, ids []string) (map[string]TimelineItem, error) {
	states := make(map[string]TimelineItem, len(ids))
	if len(ids) == 0 {
		return states, nil
	}

	rows, err := s.pool.Query(ctx, `
		select a.id::text, a.media_kind, a.sort_time, a.exif_offset_minutes,
		       a.derived_state, a.playback_state, a.duration_seconds,
		       coalesce(live.live_state, 'none')
		from assets a`+liveJoin("a")+`
		where a.id = any($1::uuid[]) and a.vault = ''`, ids)
	if err != nil {
		return nil, fmt.Errorf("query asset states: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var it TimelineItem
		var sortTime time.Time
		if err := rows.Scan(&it.ID, &it.MediaKind, &sortTime, &it.OffsetMinutes,
			&it.State, &it.PlaybackState, &it.DurationSeconds, &it.LiveState); err != nil {
			return nil, fmt.Errorf("scan asset state: %w", err)
		}
		it.TakenAt = sortTime
		it.hideEmptyLiveState()
		states[it.ID] = it
	}
	return states, rows.Err()
}

// hideEmptyLiveState drops the sentinel so a still with no motion sends no
// field at all rather than a string saying so, across every item in the page.
func (t *TimelineItem) hideEmptyLiveState() {
	if t.LiveState == PlaybackNone {
		t.LiveState = ""
	}
}
