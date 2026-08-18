package db

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// TrashRetentionDays is how long a deleted item waits before the purge can take
// it. A year is not a guess about how long anybody needs to change their mind;
// it is long enough that the answer to "did I need that?" has certainly
// arrived, and the storage a year of deletions occupies is a rounding error
// against the library it was deleted from.
const TrashRetentionDays = 365

// ErrEmptySelection means an operation named no items at all. It is refused
// rather than treated as a no-op: a delete request that resolves to nothing is
// far more likely to be a client bug than an intention.
var ErrEmptySelection = errors.New("db: the selection names no items")

// Range is a run of positions in a timeline, `Start` inclusive and `End`
// exclusive, counted in the same units TimelineAt skips in.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Selection is what an operation applies to, said either way round.
//
// Ids are exact and are what the gallery sends for anything it is actually
// holding — the tile under a right-click, the photo open in the viewer.
//
// Ranges are positions in a filtered timeline, which is the only way to name a
// selection the client has never fetched: the day table gives every photograph
// in a collection a place before any of them are downloaded, so "everything
// from here to the bottom of 2019" is one interval rather than eleven thousand
// identifiers the browser would have to page through to learn. The cost is that
// a position means something slightly different if the timeline changed since
// the day table was fetched, which is the same caveat TimelineAt carries and is
// why the first step of a delete is reversible.
//
// Both may be set. The result is their union, deduplicated.
type Selection struct {
	IDs    []string
	Ranges []Range
	Filter TimelineFilter
}

// maxRanges bounds one request. A selection is runs, not items, so even
// "everything" is a single range; a thousand disjoint runs is already far more
// than any sequence of clicks produces.
const maxRanges = 1000

// pick renders the "which assets" half of an operation: a SELECT of asset ids,
// and the arguments it needs numbered from `from`.
//
// The scope comes from the filter, so the same code resolves a position in the
// library and a position in the trash without either being able to reach into
// the other — a restore can only ever name something deleted, and a delete can
// only ever name something live.
func (sel Selection) pick(from int) (string, []any, error) {
	if len(sel.Ranges) > maxRanges {
		return "", nil, fmt.Errorf("%d ranges exceeds the %d-range limit", len(sel.Ranges), maxRanges)
	}

	scope := sel.Filter.scope()
	narrow, args, err := sel.Filter.where(from)
	if err != nil {
		return "", nil, err
	}
	next := from + len(args)

	var parts []string

	if len(sel.IDs) > 0 {
		parts = append(parts, fmt.Sprintf(
			`select a.id from assets a where a.id = any($%d::uuid[]) and %s%s`,
			next, scope, narrow))
		args = append(args, sel.IDs)
		next++
	}

	if runs := usable(sel.Ranges); len(runs) > 0 {
		los := make([]int32, len(runs))
		his := make([]int32, len(runs))
		for i, r := range runs {
			los[i], his[i] = int32(r.Start), int32(r.End)
		}
		// The row number is the position, counted in exactly the ordering
		// TimelineAt offsets into — the two have to agree or a selection made
		// in the grid would delete something else. Numbered from zero, because
		// that is what the day table's run lengths sum to.
		parts = append(parts, fmt.Sprintf(`
			select o.id from (
				select a.id, row_number() over (order by a.sort_time desc, a.id desc) - 1 as rn
				from assets a
				where %s%s
			) o
			join unnest($%d::int[], $%d::int[]) as run(lo, hi)
			  on o.rn >= run.lo and o.rn < run.hi`,
			scope, narrow, next, next+1))
		args = append(args, los, his)
	}

	if len(parts) == 0 {
		return "", nil, ErrEmptySelection
	}
	return strings.Join(parts, "\n\t\t\tunion\n"), args, nil
}

// usable drops the runs that cannot name anything.
func usable(ranges []Range) []Range {
	out := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if r.End <= r.Start || r.End <= 0 {
			continue
		}
		if r.Start < 0 {
			r.Start = 0
		}
		out = append(out, r)
	}
	return out
}

// family widens a selection to everything that has to travel with it.
//
// A Live Photo's motion and a Snapchat caption layer are not items — they are
// parts of the item somebody clicked on, invisible everywhere in the gallery
// and addressable by nothing. Leaving them behind would put half a photograph
// in the library: a still in the trash whose three seconds of video are still
// live, and a memory whose handwriting outlives the picture it was drawn on.
//
// It is written as a union over `sel` rather than a recursive walk because the
// nesting is exactly one deep in both directions and always will be: a
// component cannot itself have components.
const family = `
	fam as (
		select id from sel
		union
		select v.id from assets v join sel on v.live_parent_asset_id = sel.id
		union
		select p.overlay_asset_id as id from assets p join sel on p.id = sel.id
		where p.overlay_asset_id is not null
	)`

// TrashResult is what one delete did: the batch that undoes it, and how many
// items — not rows — went. The two differ by however many Live Photos were in
// the selection, and the number worth showing somebody is the one they can
// count on screen.
type TrashResult struct {
	Batch string
	Count int
}

// Trash moves a selection out of the library.
//
// Nothing is destroyed here and nothing is even moved: the row, the blob, the
// derivatives, the album membership and the face tags all stay exactly as they
// were, and one column decides whether the gallery can see them. Which is what
// makes the undo a single UPDATE rather than a restore, and what makes a
// misdirected delete a nuisance instead of a loss.
func (s *Store) Trash(ctx context.Context, sel Selection) (TrashResult, error) {
	sel.Filter.Trash = false

	batch, err := newBatch()
	if err != nil {
		return TrashResult{}, err
	}
	// $1 is the batch and $2 the retention; the selection's own arguments
	// follow them.
	pick, args, err := sel.pick(3)
	if err != nil {
		return TrashResult{}, err
	}
	args = append([]any{batch, int32(TrashRetentionDays)}, args...)

	// `deleted_at is null` on the update rather than in the selection: an asset
	// already in the trash keeps the batch and the expiry it went in with, so
	// deleting an overlapping selection twice cannot quietly extend or reset
	// the life of something that was deleted a month ago.
	row := s.pool.QueryRow(ctx, `
		with sel as (`+pick+`),`+family+`,
		done as (
			update assets a set deleted_at = now(),
			                    purge_after = now() + make_interval(days => $2),
			                    delete_batch = $1::uuid
			where a.id in (select id from fam) and a.deleted_at is null
			returning a.id
		)
		select count(*)::int from sel join done on done.id = sel.id`, args...)

	var count int
	if err := row.Scan(&count); err != nil {
		return TrashResult{}, fmt.Errorf("move selection to the trash: %w", err)
	}
	return TrashResult{Batch: batch, Count: count}, nil
}

// Restore brings a selection back out of the trash, into whatever it was part
// of when it left: the timeline, its albums, its categories, and — for a still
// — the motion that went down with it.
func (s *Store) Restore(ctx context.Context, sel Selection) (int, error) {
	sel.Filter.Trash = true

	pick, args, err := sel.pick(1)
	if err != nil {
		return 0, err
	}

	row := s.pool.QueryRow(ctx, `
		with sel as (`+pick+`),`+family+`,
		done as (
			update assets a set deleted_at = null, purge_after = null, delete_batch = null
			where a.id in (select id from fam) and a.deleted_at is not null
			returning a.id
		)
		select count(*)::int from sel join done on done.id = sel.id`, args...)

	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("restore selection from the trash: %w", err)
	}
	return count, nil
}

// RestoreBatch undoes one delete, exactly.
//
// This is what the toast's Undo calls, and it is deliberately not "restore what
// I last selected": between the delete and the undo the selection is gone, the
// timeline has been redrawn, and every position in it has moved. The batch is
// the only handle that still means the same thing — it names the rows that
// operation touched, including the components it carried along and excluding
// anything that was already in the trash when it ran.
//
// Idempotent: a batch that has already been restored puts nothing back.
func (s *Store) RestoreBatch(ctx context.Context, batch string) (assets, albums int, err error) {
	row := s.pool.QueryRow(ctx, `
		with restored as (
			update assets a set deleted_at = null, purge_after = null, delete_batch = null
			where a.delete_batch = $1::uuid and a.deleted_at is not null
			returning (`+notComponent+`) as item
		),
		regrouped as (
			update albums set deleted_at = null, purge_after = null, delete_batch = null
			where delete_batch = $1::uuid and deleted_at is not null
			returning 1
		)
		select (select count(*)::int from restored where item),
		       (select count(*)::int from regrouped)`, batch)

	if err := row.Scan(&assets, &albums); err != nil {
		return 0, 0, fmt.Errorf("restore batch %s: %w", batch, err)
	}
	return assets, albums, nil
}

// Purged is one asset whose row is gone and whose bytes are next. Everything
// here is what the caller needs to find the files on disk and to record what
// used to be in them.
type Purged struct {
	ID       string
	SHA256   string
	Ext      string
	MD5      string
	ByteSize int64
	Filename string
	// Item distinguishes the photograph somebody chose from the components that
	// went with it, so a count can be reported in the units it was selected in.
	Item bool
}

// PurgedItems counts the rows that were items rather than components.
func PurgedItems(purged []Purged) int {
	n := 0
	for _, p := range purged {
		if p.Item {
			n++
		}
	}
	return n
}

// purgeSelect is the tail every purge shares: delete the rows, remember the
// content, and hand back what the files were called.
//
// The tombstone is the point of doing this in one statement. A row that is gone
// and a content key that was never recorded is a photograph the next backup
// will cheerfully upload again, and the two must not be able to come apart —
// not on an error, not on a crash between two statements.
const purgeSelect = `
	gone as (
		delete from assets a where a.id in (select id from fam)
		returning a.id::text, a.sha256, a.ext, a.md5, a.byte_size, a.original_filename,
		          (` + notComponent + `) as item
	),
	tomb as (
		insert into purged_content (sha256, md5, byte_size, original_filename)
		select sha256, md5, byte_size, original_filename from gone
		on conflict (sha256) do nothing
	)
	select id, sha256, ext, md5, byte_size, original_filename, item from gone`

// Purge destroys a selection outright: rows deleted, content tombstoned, and
// the files named in the return so the caller can unlink them.
//
// Only ever called on things already in the trash. There is no path from the
// library to here, which is the whole of what makes "permanently delete" a
// second decision rather than a fast one.
//
// The bytes are the caller's to remove, because they are not the database's to
// lose: a failed unlink leaves a blob that `verify` can find and report, while
// a row deleted after its file would leave an asset the archive can no longer
// produce.
func (s *Store) Purge(ctx context.Context, sel Selection) ([]Purged, error) {
	sel.Filter.Trash = true

	pick, args, err := sel.pick(1)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		with sel as (`+pick+`),`+family+`,`+purgeSelect, args...)
	if err != nil {
		return nil, fmt.Errorf("purge selection: %w", err)
	}
	return scanPurged(rows)
}

// PurgeExpired destroys whatever has served its retention, up to a limit.
//
// The limit is not about correctness — it is one statement either way — but
// about the size of the bite. A library that has been running for a year and
// has never been swept has an unknown amount due at once, and taking it a few
// thousand rows at a time keeps one transaction from holding locks for as long
// as it takes to unlink them all.
func (s *Store) PurgeExpired(ctx context.Context, limit int) ([]Purged, error) {
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.pool.Query(ctx, `
		with sel as (
			select a.id from assets a
			where a.deleted_at is not null and a.purge_after <= now()
			order by a.purge_after
			limit $1
		),`+family+`,`+purgeSelect, limit)
	if err != nil {
		return nil, fmt.Errorf("purge expired assets: %w", err)
	}
	return scanPurged(rows)
}

func scanPurged(rows pgx.Rows) ([]Purged, error) {
	defer rows.Close()

	var out []Purged
	for rows.Next() {
		var p Purged
		if err := rows.Scan(&p.ID, &p.SHA256, &p.Ext, &p.MD5, &p.ByteSize, &p.Filename, &p.Item); err != nil {
			return nil, fmt.Errorf("scan purged asset: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PurgeExpiredAlbums drops the album rows whose retention has run out. Their
// membership went with them when they were deleted, so there is nothing on disk
// to clean up and nothing to hand back.
func (s *Store) PurgeExpiredAlbums(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		delete from albums where deleted_at is not null and purge_after <= now()`)
	if err != nil {
		return 0, fmt.Errorf("purge expired albums: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeleteAlbum removes an album, and optionally everything in it.
//
// Two operations under one batch, because they undo together. Deleting an album
// and its photographs is one thing somebody did, and an Undo that put the album
// back empty — or put the photos back into an album that no longer exists —
// would be a worse outcome than either half of it.
//
// `photos` false is the ordinary case and is not destructive at all: the album
// is a grouping an import produced, and dropping it leaves every photograph in
// it exactly where it was in the library.
func (s *Store) DeleteAlbum(ctx context.Context, id string, photos bool) (TrashResult, error) {
	batch, err := newBatch()
	if err != nil {
		return TrashResult{}, err
	}

	tag, err := s.pool.Exec(ctx, `
		update albums set deleted_at = now(),
		                  purge_after = now() + make_interval(days => $3),
		                  delete_batch = $1::uuid
		where id = $2::uuid and deleted_at is null`,
		batch, id, int32(TrashRetentionDays))
	if err != nil {
		return TrashResult{}, fmt.Errorf("delete album %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return TrashResult{}, ErrNotFound
	}

	result := TrashResult{Batch: batch}
	if !photos {
		return result, nil
	}

	// The membership is read through the album's own filter rather than
	// captured beforehand, so this deletes what was in the album at the moment
	// the album went — not what the client believed was in it.
	row := s.pool.QueryRow(ctx, `
		with sel as (
			select a.id from assets a
			where `+visibleAssets+`
			  and exists (select 1 from album_assets m
			              where m.asset_id = a.id and m.album_id = $2::uuid)
		),`+family+`,
		done as (
			update assets a set deleted_at = now(),
			                    purge_after = now() + make_interval(days => $3),
			                    delete_batch = $1::uuid
			where a.id in (select id from fam) and a.deleted_at is null
			returning a.id
		)
		select count(*)::int from sel join done on done.id = sel.id`,
		batch, id, int32(TrashRetentionDays))

	if err := row.Scan(&result.Count); err != nil {
		return TrashResult{}, fmt.Errorf("delete the photos in album %s: %w", id, err)
	}
	return result, nil
}

// TrashCount is how many items are waiting in the trash, for the row on the
// collections page that leads there.
func (s *Store) TrashCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`select count(*)::int from assets a where `+trashedAssets).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the trash: %w", err)
	}
	return n, nil
}

// PurgedContent reports which of these content keys the archive used to hold
// and deliberately does not any more.
//
// Read on the sync path beside AssetsByContent, and the reason a purge sticks:
// without it the phone would offer the bytes again on the next run and the
// server, having genuinely never seen them, would take them.
func (s *Store) PurgedContent(ctx context.Context, keys []ContentKey) (map[ContentKey]bool, error) {
	found := make(map[ContentKey]bool, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	md5s := make([]string, len(keys))
	sizes := make([]int64, len(keys))
	for i, k := range keys {
		md5s[i] = k.MD5
		sizes[i] = k.ByteSize
	}

	rows, err := s.pool.Query(ctx, `
		select distinct k.md5, k.byte_size
		from unnest($1::text[], $2::bigint[]) as k(md5, byte_size)
		join purged_content p on p.md5 = k.md5 and p.byte_size = k.byte_size`, md5s, sizes)
	if err != nil {
		return nil, fmt.Errorf("look up purged content: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key ContentKey
		if err := rows.Scan(&key.MD5, &key.ByteSize); err != nil {
			return nil, fmt.Errorf("scan purged content: %w", err)
		}
		found[key] = true
	}
	return found, rows.Err()
}

// RecordPurged tombstones content directly, for the one caller that destroys
// blobs without going through Purge: a rebuild replaying a manifest that says
// these bytes were thrown away.
func (s *Store) RecordPurged(ctx context.Context, entries []Purged) error {
	if len(entries) == 0 {
		return nil
	}

	shas := make([]string, len(entries))
	md5s := make([]string, len(entries))
	sizes := make([]int64, len(entries))
	names := make([]string, len(entries))
	for i, e := range entries {
		shas[i], md5s[i], sizes[i], names[i] = e.SHA256, e.MD5, e.ByteSize, e.Filename
	}

	_, err := s.pool.Exec(ctx, `
		insert into purged_content (sha256, md5, byte_size, original_filename)
		select p.sha256, p.md5, p.byte_size, p.filename
		from unnest($1::text[], $2::text[], $3::bigint[], $4::text[])
		     as p(sha256, md5, byte_size, filename)
		on conflict (sha256) do nothing`, shas, md5s, sizes, names)
	if err != nil {
		return fmt.Errorf("record purged content: %w", err)
	}
	return nil
}

// IsPurged reports whether these digests name content the archive threw away.
// The sha256 side of the same question PurgedContent answers by (md5, size),
// asked by a rebuild, which knows the real digest of every blob it is holding.
func (s *Store) IsPurged(ctx context.Context, shas []string) (map[string]bool, error) {
	out := make(map[string]bool, len(shas))
	if len(shas) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx,
		`select sha256 from purged_content where sha256 = any($1::text[])`, shas)
	if err != nil {
		return nil, fmt.Errorf("look up purged digests: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("scan purged digest: %w", err)
		}
		out[sha] = true
	}
	return out, rows.Err()
}

// NewBatch is newBatch for the callers outside this package that need one: an
// album or a person going into the vault with nothing in it still needs a
// handle for its Undo to name.
func NewBatch() (string, error) { return newBatch() }

// newBatch mints the identifier that ties one delete to its undo.
//
// A uuid because the column is one, and generated here rather than by
// gen_random_uuid() so the value is known before the statement runs and can be
// handed back whatever the statement matched — including nothing.
func newBatch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate delete batch: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// DropBySHA256 removes asset rows by digest, and tombstones what they held.
//
// The rebuild's counterpart to Purge. A manifest replay reaches this having read
// a purge line rather than having selected anything, so it names content instead
// of items — and it has to be able to reach a row in either half of the archive,
// because a purge line retracts an asset line without caring whether the row a
// half-finished rebuild put back was live or trashed.
func (s *Store) DropBySHA256(ctx context.Context, shas []string) (int, error) {
	if len(shas) == 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, `
		with sel as (select a.id from assets a where a.sha256 = any($1::text[])),`+family+`
		delete from assets where id in (select id from fam)`, shas)
	if err != nil {
		return 0, fmt.Errorf("drop purged assets: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
