package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The two buckets. Functionally one mechanism; separate because the reasons a
// person has for each are separate, and a single destination with a flag on it
// would be the kind of tidiness that makes a product worse.
const (
	VaultArchive = "archive"
	VaultHidden  = "hidden"
)

// ErrBadBucket names a vault that does not exist.
var ErrBadBucket = errors.New("db: unknown vault bucket")

// ErrNoVaultSecret means nothing has ever been hidden on this archive, so there
// is no keypair and no password yet.
var ErrNoVaultSecret = errors.New("db: this archive has no vault")

func ValidBucket(bucket string) bool {
	return bucket == VaultArchive || bucket == VaultHidden
}

// vaultedAssets is the third scope, beside visibleAssets and trashedAssets.
//
// It excludes the trash rather than ignoring it, which is not redundant with
// the column check: an item cannot be in both, and writing it down here is what
// makes that a property of the query rather than a thing that happens to be
// true because of how the operations are ordered.
const vaultedAssets = notComponent + ` and a.vault <> '' and a.deleted_at is null`

// scrubbed is every column the vault takes out of the assets row, and the value
// the row is left holding.
//
// One list, used twice, because a column that is emptied on the way in and not
// filled on the way out is a photograph that comes back without its camera —
// and the two lists would drift the first time a migration added a tag. See
// scrubSet and restoreSet, which are both generated from this.
//
// What is deliberately *not* here is the short list of things a locked archive
// still has to know:
//
//   - sha256, md5 and byte_size, because sync/check answers "have I got this?"
//     from the content key. Without them the phone would offer the photograph
//     again on the next backup and the archive, having genuinely forgotten it,
//     would take it — hiding a photograph would restore it. The cost is real
//     and worth naming: somebody holding the database can test whether a file
//     they already have is in the vault. They cannot learn anything they did
//     not already have.
//   - media_kind, because the pipeline's invariants are written in terms of it
//     and a row that lies about being a video is worse than one bit of leakage.
//   - the structural columns — live_parent_*, is_overlay, overlay_asset_id —
//     because they are what says this row is part of another one, and the
//     family that travels with a selection is resolved through them.
//   - device_id, local_id and uploaded_at, which describe the delivery rather
//     than the photograph.
var scrubbed = []struct{ Column, Blank string }{
	{"original_filename", "''"},
	{"ext", "''"},
	{"content_type", "''"},
	{"content_id", "''"},
	{"captured_at", "null"},
	{"width", "null"},
	{"height", "null"},
	{"orientation", "null"},
	{"duration_seconds", "null"},
	{"camera_make", "null"},
	{"camera_model", "null"},
	{"lens", "null"},
	{"gps_lat", "null"},
	{"gps_lon", "null"},
	// Derived from the coordinates above rather than read off the file, and
	// emptied for the same reason they are — more so, because "Chicago" is
	// legible at a glance in a way 41.85,-87.65 is not.
	{"place_city", "null"},
	{"place_admin1", "null"},
	{"place_country", "null"},
	{"place_source", "null"},
	{"geocoded_at", "null"},
	{"exif_captured_at", "null"},
	{"exif_offset_minutes", "null"},
	{"description", "null"},
	{"favorite", "false"},
	{"archived", "false"},
	{"import_source", "''"},
	{"import_metadata", "null"},
	{"import_gps_lat", "null"},
	{"import_gps_lon", "null"},
	{"exif_metadata", "null"},
	{"gps_altitude", "null"},
	{"gps_direction", "null"},
	{"gps_accuracy", "null"},
	{"gps_at", "null"},
	{"iso", "null"},
	{"f_number", "null"},
	{"exposure_seconds", "null"},
	{"focal_length", "null"},
	{"focal_length_35", "null"},
	{"flash", "null"},
	{"exif_description", "null"},
	{"color_profile", "null"},
	{"capture_type", "null"},
	{"video_codec", "null"},
	{"frame_rate", "null"},
	{"bitrate", "null"},
	{"audio_codec", "null"},
	{"audio_channels", "null"},
	{"faces", "null"},
	{"subtypes", "'{}'"},
}

// scrubSet renders "column = blank, ..." — what the row is left saying.
func scrubSet() string {
	parts := make([]string, len(scrubbed))
	for i, c := range scrubbed {
		parts[i] = c.Column + " = " + c.Blank
	}
	return strings.Join(parts, ", ")
}

// restoreSet renders "column = d.column, ..." against a record built from the
// sealed document, which is the exact inverse.
func restoreSet() string {
	parts := make([]string, len(scrubbed))
	for i, c := range scrubbed {
		parts[i] = c.Column + " = d." + c.Column
	}
	return strings.Join(parts, ", ")
}

// VaultCandidate is one asset on its way into the vault: what the caller needs
// to find its files, and the document it has to seal before the row is emptied.
type VaultCandidate struct {
	AssetID string
	SHA256  string
	Ext     string
	// Doc is the whole row as JSON, plus the albums and people it belonged to.
	// Read here rather than assembled from a struct so that a column added by a
	// future migration is carried without this file having heard of it.
	Doc json.RawMessage
	// Analysis is what the ML passes wrote about it, or nil if nothing had.
	// Sealed separately from Doc — see analysisDoc.
	Analysis json.RawMessage
	// Item distinguishes the photograph somebody chose from the components that
	// went with it, so a count can be reported in the units it was selected in.
	Item bool
}

// SealedItem is a candidate after the caller has encrypted it.
type SealedItem struct {
	AssetID string
	Sealed  []byte
	// SealedAnalysis is the encrypted analysisDoc, or nil when there was none.
	SealedAnalysis []byte
}

// vaultDoc builds the sealed document for one row.
//
// `to_jsonb(a)` rather than a column list, deliberately. The document has to
// carry everything a restore puts back, and a hand-written list is a list that
// is wrong the first time a migration adds a tag — at which point a photograph
// taken out of the vault would come back missing whatever was added while it
// was in there. The albums come as id and title together so a restore can put
// the photograph back into the album it names *if that album still exists*, and
// recognise that it does not if it has since been deleted.
const vaultDoc = `
	jsonb_build_object(
		'v', 1,
		'asset', to_jsonb(a),
		'albums', coalesce((
			select jsonb_agg(jsonb_build_object('id', al.id, 'title', al.title, 'source', al.source))
			from album_assets m join albums al on al.id = m.album_id
			where m.asset_id = a.id
		), '[]'::jsonb),
		'people', coalesce((
			select jsonb_agg(p.name order by p.name) from asset_people p where p.asset_id = a.id
		), '[]'::jsonb)
	)`

// analysisDoc is the second sealed document: everything the ML passes wrote
// about one photograph.
//
// Its own document rather than another member of vaultDoc, and the reason is
// the one internal/api/analysis.go gives for that endpoint being its own route.
// vault.Index opens every document in a bucket to draw the gallery, where a
// caption is a sentence, an OCR row is however many kilobytes a screenshot of a
// terminal came to, and an embedding is 1152 numbers per sampled frame. One
// document would mean paying all of that to draw a grid of thumbnails. Restore
// is the only thing that ever reads this one.
//
// Null when the four tables have nothing to say, which on a library the
// captioner has not been run over is most of it — and null is the honest way to
// record "nothing had been written", as distinct from "a document was sealed
// and it was empty".
//
// The tags travel as names rather than ids. tags is the shared vocabulary and
// nothing here deletes from it, so the id would probably still resolve — but
// "probably" is not what a restore should rest on, and a name is what the row
// meant anyway. canonical_id is deliberately not read: asset_tags records what
// the model actually wrote, the merge is resolved on the way out, and a
// document that stored the merged word would bake a merge into rows that could
// then not be unmerged.
//
// The vectors travel as their text form, which pgvector round-trips exactly.
const analysisDoc = `(
	select jsonb_build_object(
		'v', 1,
		'captions', coalesce((
			select jsonb_agg(jsonb_build_object(
				'model', d.model, 'caption', d.caption, 'generated_at', d.generated_at))
			from asset_descriptions d where d.asset_id = a.id
		), '[]'::jsonb),
		'ocr', coalesce((
			select jsonb_agg(jsonb_build_object(
				'model', o.model, 'text', o.text, 'generated_at', o.generated_at))
			from asset_ocr o where o.asset_id = a.id
		), '[]'::jsonb),
		'tags', coalesce((
			select jsonb_agg(jsonb_build_object(
				'name', tg.name, 'confidence', at.confidence))
			from asset_tags at join tags tg on tg.id = at.tag_id
			where at.asset_id = a.id
		), '[]'::jsonb),
		'embeddings', coalesce((
			select jsonb_agg(jsonb_build_object(
				'frame', e.frame, 'model', e.model, 'vector', e.embedding::text))
			from asset_embeddings e where e.asset_id = a.id
		), '[]'::jsonb))
	where exists (select 1 from asset_descriptions d where d.asset_id = a.id)
	   or exists (select 1 from asset_ocr o where o.asset_id = a.id)
	   or exists (select 1 from asset_tags at where at.asset_id = a.id)
	   or exists (select 1 from asset_embeddings e where e.asset_id = a.id)
)`

// analysisTables is every table the vault empties of words, in the order a
// foreign key would want if one of them ever pointed at another.
//
// A list rather than five statements written out, for the reason `scrubbed` is
// a list: the delete on the way in and the sweep that catches what an older
// build left behind are the same set, and two copies of it would drift the
// first time the ML pipeline grew a table. asset_search is here too even though
// it is derived — it is derived from rows that are about to be gone, and a
// stale tsvector is the most legible of all of them.
var analysisTables = []string{
	"asset_descriptions",
	"asset_ocr",
	"asset_tags",
	"asset_embeddings",
	"asset_search",
}

// VaultCandidates resolves a selection to the rows that are about to be hidden,
// widened to the components that have to travel with them.
//
// A read, and only a read. The caller encrypts the files and seals the
// documents before anything is written, so a failure anywhere in that stretch
// leaves the library exactly as it was — which is why hiding is two steps and
// deleting, which touches nothing on disk, is one.
func (s *Store) VaultCandidates(ctx context.Context, sel Selection) ([]VaultCandidate, error) {
	sel.Filter.Trash = false

	pick, args, err := sel.pick(1)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		with sel as (`+pick+`),`+family+`
		select a.id::text, a.sha256, a.ext, `+vaultDoc+`, `+analysisDoc+`, (`+notComponent+`) as item
		from assets a where a.id in (select id from fam)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read vault candidates: %w", err)
	}
	return scanCandidates(rows)
}

// VaultAlbumCandidates is the same read for every photograph in one album.
//
// The membership is read through the album's own filter at the moment the
// operation runs, rather than captured by the client beforehand: hiding an
// album hides what is in it now.
func (s *Store) VaultAlbumCandidates(ctx context.Context, albumID string) ([]VaultCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		with sel as (
			select a.id from assets a
			where `+visibleAssets+`
			  and exists (select 1 from album_assets m
			              where m.asset_id = a.id and m.album_id = $1::uuid)
		),`+family+`
		select a.id::text, a.sha256, a.ext, `+vaultDoc+`, `+analysisDoc+`, (`+notComponent+`) as item
		from assets a where a.id in (select id from fam)`, albumID)
	if err != nil {
		return nil, fmt.Errorf("read the photos in album %s: %w", albumID, err)
	}
	return scanCandidates(rows)
}

// VaultPersonCandidates is the same read for every photograph a name is tagged
// on.
func (s *Store) VaultPersonCandidates(ctx context.Context, name string) ([]VaultCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		with sel as (
			select a.id from assets a
			where `+visibleAssets+`
			  and exists (select 1 from asset_people p
			              where p.asset_id = a.id and p.name = $1)
		),`+family+`
		select a.id::text, a.sha256, a.ext, `+vaultDoc+`, `+analysisDoc+`, (`+notComponent+`) as item
		from assets a where a.id in (select id from fam)`, name)
	if err != nil {
		return nil, fmt.Errorf("read the photos of %q: %w", name, err)
	}
	return scanCandidates(rows)
}

func scanCandidates(rows pgx.Rows) ([]VaultCandidate, error) {
	defer rows.Close()

	var out []VaultCandidate
	for rows.Next() {
		var c VaultCandidate
		if err := rows.Scan(&c.AssetID, &c.SHA256, &c.Ext, &c.Doc, &c.Analysis, &c.Item); err != nil {
			return nil, fmt.Errorf("scan vault candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// VaultResult is what one operation moved, and the batch that undoes it.
type VaultResult struct {
	Batch string
	Count int
}

// CommitVault empties the rows and stores what was in them, in one transaction.
//
// The order inside it is the whole design. The sealed document goes in first,
// so there is never an instant where a row has been emptied and nothing holds
// what it said. Then the row is scrubbed and marked. Then the membership rows
// go — which is the "removed from its albums, its people and its categories"
// half of the feature, and it is a delete rather than a flag because a row in
// asset_people naming a hidden photograph is exactly the fact the vault exists
// to withhold. The categories need no statement at all: every one of them is a
// predicate over columns this scrub has just emptied, inside a scope this row
// has just left.
//
// The jobs go too. A pending thumbnail for a photograph whose plaintext is
// about to be deleted is a job that can only fail, five times, and then mark a
// hidden photograph broken.
//
// And last the words: the caption, the recognised text, the tags, the vectors
// and the tsvector built out of them, sealed into a second document first and
// then deleted, in that order and for the reason the row's document goes in
// first. The write paths have refused to describe a hidden photograph since
// they were written, which was never the same thing as this — the ordinary
// order of operations is that the backfill runs over the library and things
// get hidden afterwards, so what mattered was always the rows written before
// the hide, and until this they simply stayed. See analysisDoc and migration
// 0023.
func (s *Store) CommitVault(ctx context.Context, bucket string, items []SealedItem) (VaultResult, error) {
	if !ValidBucket(bucket) {
		return VaultResult{}, fmt.Errorf("%w: %q", ErrBadBucket, bucket)
	}
	if len(items) == 0 {
		return VaultResult{}, ErrEmptySelection
	}
	batch, err := newBatch()
	if err != nil {
		return VaultResult{}, err
	}

	ids := make([]string, len(items))
	sealed := make([][]byte, len(items))
	analyses := make([][]byte, len(items))
	for i, it := range items {
		ids[i], sealed[i], analyses[i] = it.AssetID, it.Sealed, it.SealedAnalysis
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return VaultResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// `do nothing` rather than `do update`. A selection cannot name something
	// already in the vault — VaultCandidates resolves in the library — so this
	// only fires on a retry, and on a retry the row it would seal has already
	// been scrubbed. Overwriting a good document with a description of an empty
	// row is the one way this operation could lose a photograph's metadata for
	// good, and the cheapest way not to is never to overwrite.
	if _, err := tx.Exec(ctx, `
		insert into vault_items (asset_id, sealed, sealed_analysis)
		select k.id::uuid, k.sealed, k.analysis
		from unnest($1::text[], $2::bytea[], $3::bytea[]) as k(id, sealed, analysis)
		on conflict (asset_id) do nothing`,
		ids, sealed, analyses); err != nil {
		return VaultResult{}, fmt.Errorf("store sealed metadata: %w", err)
	}

	// `vault = ''` on the update rather than in the selection, the same guard
	// the trash uses: hiding an overlapping selection twice must not re-stamp a
	// batch onto something that went in last month, or its Undo would drag that
	// back out too.
	//
	// The count is of items rather than rows, because a Live Photo is one thing
	// somebody selected and two rows that had to move.
	var count int
	if err := tx.QueryRow(ctx, `
		with done as (
			update assets a set vault = $2, vaulted_at = now(), vault_batch = $3::uuid, `+scrubSet()+`
			where a.id = any($1::uuid[]) and a.vault = ''
			returning (`+notComponent+`) as item
		)
		select count(*) filter (where item)::int from done`,
		ids, bucket, batch).Scan(&count); err != nil {
		return VaultResult{}, fmt.Errorf("move the selection into the vault: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`delete from album_assets where asset_id = any($1::uuid[])`, ids); err != nil {
		return VaultResult{}, fmt.Errorf("remove the hidden photos from their albums: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`delete from asset_people where asset_id = any($1::uuid[])`, ids); err != nil {
		return VaultResult{}, fmt.Errorf("remove the hidden photos from their people: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`delete from jobs where asset_id = any($1::uuid[]) and state <> 'running'`, ids); err != nil {
		return VaultResult{}, fmt.Errorf("drop the queued work for hidden photos: %w", err)
	}
	if err := dropAnalysis(ctx, tx, ids); err != nil {
		return VaultResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return VaultResult{}, fmt.Errorf("commit the vault operation: %w", err)
	}
	return VaultResult{Batch: batch, Count: count}, nil
}

// dropAnalysis takes the words out, inside whichever transaction sealed them.
//
// Unconditional on the ids it is given rather than joined against the vault
// column, because both callers have just established that in the same transaction —
// CommitVault by scrubbing the rows, ReconcileAnalysis by its own guard. A join
// here would be a second opinion about a fact the caller owns.
func dropAnalysis(ctx context.Context, tx pgx.Tx, ids []string) error {
	for _, table := range analysisTables {
		if _, err := tx.Exec(ctx,
			`delete from `+table+` where asset_id = any($1::uuid[])`, ids); err != nil {
			return fmt.Errorf("take %s away from the hidden photos: %w", table, err)
		}
	}
	return nil
}

// restoreAnalysis puts one photograph's words back from its opened document.
//
// Every insert is `do nothing` on conflict rather than an update: nothing can
// have written a row for this asset while it was hidden — the guards on the
// write paths are what make that true — so a conflict here means this restore
// has already run once and the row that is there is this one.
//
// The tags are interned before they are linked, the way putTags interns them,
// because `tags` is the shared vocabulary and a word whose only photograph was
// hidden may have been the last thing holding its row... which is precisely why
// nothing deletes from that table. Interning is what makes this correct anyway
// if something one day does.
func restoreAnalysis(ctx context.Context, tx pgx.Tx, assetID string, doc []byte) error {
	if len(doc) == 0 {
		return nil
	}

	const captions = `
		insert into asset_descriptions (asset_id, model, caption, generated_at)
		select $1::uuid, e->>'model', e->>'caption', (e->>'generated_at')::timestamptz
		from jsonb_array_elements($2::jsonb -> 'captions') e
		on conflict (asset_id, model) do nothing`
	if _, err := tx.Exec(ctx, captions, assetID, doc); err != nil {
		return fmt.Errorf("give %s back its caption: %w", assetID, err)
	}

	const ocr = `
		insert into asset_ocr (asset_id, model, text, generated_at)
		select $1::uuid, e->>'model', e->>'text', (e->>'generated_at')::timestamptz
		from jsonb_array_elements($2::jsonb -> 'ocr') e
		on conflict (asset_id, model) do nothing`
	if _, err := tx.Exec(ctx, ocr, assetID, doc); err != nil {
		return fmt.Errorf("give %s back its recognised text: %w", assetID, err)
	}

	const intern = `
		insert into tags (name)
		select distinct e->>'name' from jsonb_array_elements($1::jsonb -> 'tags') e
		on conflict (name) do nothing`
	if _, err := tx.Exec(ctx, intern, doc); err != nil {
		return fmt.Errorf("intern the tag names of %s: %w", assetID, err)
	}

	const link = `
		insert into asset_tags (asset_id, tag_id, confidence)
		select $1::uuid, t.id, (e->>'confidence')::real
		from jsonb_array_elements($2::jsonb -> 'tags') e
		join tags t on t.name = e->>'name'
		on conflict (asset_id, tag_id) do nothing`
	if _, err := tx.Exec(ctx, link, assetID, doc); err != nil {
		return fmt.Errorf("give %s back its tags: %w", assetID, err)
	}

	const embeddings = `
		insert into asset_embeddings (asset_id, frame, model, embedding)
		select $1::uuid, (e->>'frame')::int, e->>'model', (e->>'vector')::halfvec
		from jsonb_array_elements($2::jsonb -> 'embeddings') e
		on conflict (asset_id, frame, model) do nothing`
	if _, err := tx.Exec(ctx, embeddings, assetID, doc); err != nil {
		return fmt.Errorf("give %s back its vectors: %w", assetID, err)
	}
	return nil
}

// VaultedRow is one sealed document as it comes back out.
//
// SealedAnalysis is read only where it is needed, which is the restore. The
// gallery deliberately does not carry it — see analysisDoc — so it is nil on
// every row VaultRows returns and that is not a row with nothing to say.
type VaultedRow struct {
	AssetID        string
	Bucket         string
	Sealed         []byte
	SealedAnalysis []byte
}

// VaultRows reads the sealed documents in one bucket, or in both when bucket is
// empty.
//
// This is the whole of how the vault's own gallery is built. The rows are
// opened in memory by something holding the private key, and the timeline, the
// day table and the collections are computed from the result — none of which
// SQL could do, because every column those questions are about has been
// encrypted. See internal/vault.Index.
//
// Which is affordable because a vault is small in a way a library is not: it is
// the photographs somebody deliberately went and hid, counted in hundreds.
func (s *Store) VaultRows(ctx context.Context, bucket string) ([]VaultedRow, error) {
	if bucket != "" && !ValidBucket(bucket) {
		return nil, fmt.Errorf("%w: %q", ErrBadBucket, bucket)
	}

	rows, err := s.pool.Query(ctx, `
		select a.id::text, a.vault, v.sealed
		from assets a join vault_items v on v.asset_id = a.id
		where `+vaultedAssets+` and ($1 = '' or a.vault = $1)
		order by a.vaulted_at desc, a.id desc`, bucket)
	if err != nil {
		return nil, fmt.Errorf("read the vault: %w", err)
	}
	defer rows.Close()

	var out []VaultedRow
	for rows.Next() {
		var r VaultedRow
		if err := rows.Scan(&r.AssetID, &r.Bucket, &r.Sealed); err != nil {
			return nil, fmt.Errorf("scan vault row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VaultSealed reads one sealed document, for a restore that names ids.
func (s *Store) VaultSealed(ctx context.Context, ids []string) ([]VaultedRow, error) {
	rows, err := s.pool.Query(ctx, `
		select a.id::text, a.vault, v.sealed, v.sealed_analysis
		from assets a join vault_items v on v.asset_id = a.id
		where a.id = any($1::uuid[]) and a.vault <> ''`, ids)
	if err != nil {
		return nil, fmt.Errorf("read sealed metadata: %w", err)
	}
	defer rows.Close()

	var out []VaultedRow
	for rows.Next() {
		var r VaultedRow
		if err := rows.Scan(&r.AssetID, &r.Bucket, &r.Sealed, &r.SealedAnalysis); err != nil {
			return nil, fmt.Errorf("scan vault row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResealVault replaces the sealed documents of items already in the vault.
//
// The one write in this package that rewrites a document rather than making or
// destroying one, and it exists for exactly one reason: album membership. A
// hidden photograph's albums are inside its sealed document — that is where
// CommitVault put them, and it is what a restore reads — so putting a hidden
// photograph into a hidden album means opening the document, adding a line, and
// sealing it again. There is no column to update.
//
// Unlike the insert in CommitVault this deliberately does overwrite, because
// here the new document is the better one: the caller has just opened the old
// one to build it. The `vault <> ”` guard is what keeps it from writing a
// document over a row that has been restored out from under it in the meantime.
func (s *Store) ResealVault(ctx context.Context, items []SealedItem) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	sealed := make([][]byte, len(items))
	for i, it := range items {
		ids[i], sealed[i] = it.AssetID, it.Sealed
	}

	if _, err := s.pool.Exec(ctx, `
		update vault_items v set sealed = k.sealed
		from unnest($1::text[], $2::bytea[]) as k(id, sealed)
		join assets a on a.id = k.id::uuid and a.vault <> ''
		where v.asset_id = k.id::uuid`, ids, sealed); err != nil {
		return fmt.Errorf("reseal metadata for %d items: %w", len(items), err)
	}
	return nil
}

// Restoration is one photograph on its way back, opened.
type Restoration struct {
	AssetID string
	// Asset is the `asset` member of the sealed document: the row as it was.
	Asset json.RawMessage
	// AlbumIDs are the albums it was in. An id naming an album that has since
	// been deleted is dropped rather than reported — see CommitUnvault.
	AlbumIDs []string
	People   []string
	// Analysis is the opened analysis document, or nil when nothing had been
	// written about this photograph before it was hidden.
	Analysis json.RawMessage
	Item     bool
}

// CommitUnvault writes the opened documents back into the rows they came out
// of, and puts the memberships back where they still have somewhere to go.
//
// The album membership is an insert with a join rather than a plain insert, and
// that join is the answer to the one genuinely ambiguous case in this feature:
// a photograph hidden out of an album that was deleted while it was in the
// vault. There is no album to go back to, so it goes back to the library and
// nothing else happens. No warning, no resurrected album, no error — the album
// is gone, and being told about it at the moment of a restore would be a
// notification about something somebody did on purpose weeks ago.
func (s *Store) CommitUnvault(ctx context.Context, items []Restoration) (int, error) {
	if len(items) == 0 {
		return 0, ErrEmptySelection
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids := make([]string, len(items))
	docs := make([][]byte, len(items))
	for i, it := range items {
		ids[i], docs[i] = it.AssetID, it.Asset
	}

	// jsonb_populate_record builds a row of the assets type out of the sealed
	// document, so the update names the columns once and the values come from
	// whatever the document happens to hold. A document sealed before a
	// migration added a column simply has nothing to say about it, and the
	// column keeps the value the scrub left, which is its default.
	if _, err := tx.Exec(ctx, `
		update assets a set vault = '', vaulted_at = null, vault_batch = null, `+restoreSet()+`
		from unnest($1::text[], $2::jsonb[]) as k(id, doc),
		     lateral jsonb_populate_record(null::assets, k.doc) d
		where a.id = k.id::uuid and a.id::text = k.id and a.vault <> ''`,
		ids, docs); err != nil {
		return 0, fmt.Errorf("restore the rows from the vault: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`delete from vault_items where asset_id = any($1::uuid[])`, ids); err != nil {
		return 0, fmt.Errorf("drop the sealed metadata: %w", err)
	}

	for _, it := range items {
		if len(it.AlbumIDs) > 0 {
			if _, err := tx.Exec(ctx, `
				insert into album_assets (album_id, asset_id)
				select al.id, $1::uuid from albums al
				where al.id = any($2::uuid[]) and al.deleted_at is null and al.vault = ''
				on conflict do nothing`, it.AssetID, it.AlbumIDs); err != nil {
				return 0, fmt.Errorf("put %s back into its albums: %w", it.AssetID, err)
			}
		}
		if len(it.People) > 0 {
			if _, err := tx.Exec(ctx, `
				insert into asset_people (asset_id, name)
				select $1::uuid, name from unnest($2::text[]) as name
				on conflict do nothing`, it.AssetID, it.People); err != nil {
				return 0, fmt.Errorf("put %s back under its people: %w", it.AssetID, err)
			}
		}
		if err := restoreAnalysis(ctx, tx, it.AssetID, it.Analysis); err != nil {
			return 0, err
		}
	}

	// And the tsvector, rebuilt rather than carried, because it is the one part
	// of the analysis that is derived: the recipe lives in migration 0018 and a
	// stored copy sealed under an older one would come back stale. Run for
	// every restored id and not only the ones that had a caption — a filename
	// and a place name are a searchable row on their own, and that row went
	// when the photograph did.
	if err := refresh(ctx, tx, ids); err != nil {
		return 0, err
	}

	// A restored photograph whose derivatives never finished gets the job back.
	// Everything it needs is on disk again by the time this commits.
	if _, err := tx.Exec(ctx, `
		insert into jobs (kind, asset_id)
		select 'metadata', a.id from assets a
		where a.id = any($1::uuid[]) and a.derived_state <> 'ready'
		on conflict (asset_id, kind) do update
			set state = 'pending', run_after = now(), attempts = 0, last_error = null`,
		ids); err != nil {
		return 0, fmt.Errorf("requeue derivatives for restored photos: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the restore: %w", err)
	}

	count := 0
	for _, it := range items {
		if it.Item {
			count++
		}
	}
	return count, nil
}

// VaultAlbum moves an album's row into a bucket. The photographs are moved by
// the caller, under the same batch, because they are the part that needs a key.
func (s *Store) VaultAlbum(ctx context.Context, id, bucket, batch string) error {
	if !ValidBucket(bucket) {
		return fmt.Errorf("%w: %q", ErrBadBucket, bucket)
	}
	tag, err := s.pool.Exec(ctx, `
		update albums set vault = $2, vaulted_at = now(), vault_batch = $3::uuid
		where id = $1::uuid and vault = '' and deleted_at is null`, id, bucket, batch)
	if err != nil {
		return fmt.Errorf("move album %s into the vault: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// VaultPerson records that a name is hidden.
//
// The row is what makes this outlive its photographs. Without it, hiding
// somebody would be indistinguishable from every photograph of them happening
// to be hidden, and the day an import tagged that name onto a new photograph
// they would quietly reappear in the library.
func (s *Store) VaultPerson(ctx context.Context, name, bucket, batch string) error {
	if !ValidBucket(bucket) {
		return fmt.Errorf("%w: %q", ErrBadBucket, bucket)
	}
	_, err := s.pool.Exec(ctx, `
		insert into vault_people (name, vault, vault_batch) values ($1, $2, $3::uuid)
		on conflict (name) do update set vault = excluded.vault, vaulted_at = now(),
		                                 vault_batch = excluded.vault_batch`,
		name, bucket, batch)
	if err != nil {
		return fmt.Errorf("hide %q: %w", name, err)
	}
	return nil
}

// UnvaultAlbum and UnvaultPerson take the grouping back out. The photographs
// follow separately, for the same reason they went in separately.
func (s *Store) UnvaultAlbum(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`update albums set vault = '', vaulted_at = null, vault_batch = null where id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("take album %s out of the vault: %w", id, err)
	}
	return nil
}

func (s *Store) UnvaultPerson(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `delete from vault_people where name = $1`, name)
	if err != nil {
		return fmt.Errorf("take %q out of the vault: %w", name, err)
	}
	return nil
}

// VaultedAlbums lists the album rows in one bucket. Titles are in the clear —
// see migration 0012 for the asymmetry and what it costs.
func (s *Store) VaultedAlbums(ctx context.Context, bucket string) ([]Album, error) {
	rows, err := s.pool.Query(ctx, `
		select al.id::text, al.source, al.title, al.description
		from albums al where al.vault = $1 and al.deleted_at is null
		order by al.title`, bucket)
	if err != nil {
		return nil, fmt.Errorf("read vaulted albums: %w", err)
	}
	defer rows.Close()

	albums := []Album{}
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Source, &a.Title, &a.Description); err != nil {
			return nil, fmt.Errorf("scan vaulted album: %w", err)
		}
		albums = append(albums, a)
	}
	return albums, rows.Err()
}

// VaultedPeople lists the names hidden into one bucket.
func (s *Store) VaultedPeople(ctx context.Context, bucket string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`select name from vault_people where vault = $1 order by name`, bucket)
	if err != nil {
		return nil, fmt.Errorf("read vaulted people: %w", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan vaulted person: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// VaultBatch names everything one operation touched, which is what an Undo is
// pointed at — the same handle a delete's batch is, and for the same reason:
// between the operation and the click the grid has been redrawn and every
// position in it means something else.
func (s *Store) VaultBatch(ctx context.Context, batch string) (assetIDs []string, albumIDs []string, people []string, err error) {
	rows, err := s.pool.Query(ctx,
		`select id::text from assets where vault_batch = $1::uuid and vault <> ''`, batch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read vault batch: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, nil, fmt.Errorf("scan vault batch: %w", err)
		}
		assetIDs = append(assetIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	albumRows, err := s.pool.Query(ctx,
		`select id::text from albums where vault_batch = $1::uuid and vault <> ''`, batch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read vault batch albums: %w", err)
	}
	for albumRows.Next() {
		var id string
		if err := albumRows.Scan(&id); err != nil {
			albumRows.Close()
			return nil, nil, nil, fmt.Errorf("scan vault batch album: %w", err)
		}
		albumIDs = append(albumIDs, id)
	}
	albumRows.Close()

	peopleRows, err := s.pool.Query(ctx,
		`select name from vault_people where vault_batch = $1::uuid`, batch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read vault batch people: %w", err)
	}
	defer peopleRows.Close()
	for peopleRows.Next() {
		var name string
		if err := peopleRows.Scan(&name); err != nil {
			return nil, nil, nil, fmt.Errorf("scan vault batch person: %w", err)
		}
		people = append(people, name)
	}
	return assetIDs, albumIDs, people, peopleRows.Err()
}

// VaultCounts is how much is in each bucket.
//
// Read by the server and, while the vault is locked, deliberately not sent
// anywhere: a locked vault says "Locked" rather than "Locked, 41 items". It is
// here because an unlocked one does show it, and because the sweep that
// reconciles plaintext left behind needs to know whether there is anything to
// reconcile.
func (s *Store) VaultCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		select a.vault, count(*)::int from assets a
		where `+vaultedAssets+` group by a.vault`)
	if err != nil {
		return nil, fmt.Errorf("count the vault: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{VaultArchive: 0, VaultHidden: 0}
	for rows.Next() {
		var bucket string
		var n int
		if err := rows.Scan(&bucket, &n); err != nil {
			return nil, fmt.Errorf("scan vault count: %w", err)
		}
		counts[bucket] = n
	}
	return counts, rows.Err()
}

// VaultFile names a file that is in the vault, for the sweep and for `verify`.
type VaultFile struct {
	AssetID string
	SHA256  string
	Bucket  string
}

// VaultFiles lists every asset currently in the vault, by digest.
//
// The sweep uses it to find plaintext that a crash between "the row is hidden"
// and "the plaintext is gone" would have left on the archive drive — which is
// the one window in this feature where a hidden photograph is still readable,
// and the reason the sweep exists rather than being an optimisation.
func (s *Store) VaultFiles(ctx context.Context) ([]VaultFile, error) {
	rows, err := s.pool.Query(ctx,
		`select id::text, sha256, vault from assets where vault <> ''`)
	if err != nil {
		return nil, fmt.Errorf("list vault files: %w", err)
	}
	defer rows.Close()

	var out []VaultFile
	for rows.Next() {
		var f VaultFile
		if err := rows.Scan(&f.AssetID, &f.SHA256, &f.Bucket); err != nil {
			return nil, fmt.Errorf("scan vault file: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// VaultAnalysisLeftBehind names hidden photographs that still have words in
// the clear, and the document that would take them away.
//
// The counterpart of VaultFiles, and it exists for the same reason: a hidden
// photograph whose caption is still in asset_descriptions is the same kind of
// finding as one whose original is still in the blob tree, and neither is
// something to leave until somebody notices. Two things put a row here. One is
// an archive that hid photographs before migration 0023 existed, which is every
// archive that has ever used this feature. The other is a crash between
// CommitVault's two statements, which cannot happen because they are one
// transaction — named anyway, because "cannot happen" is how the plaintext
// sweep would have been described too.
//
// A read, and only a read. The caller seals what it gets back before anything
// is deleted, which is the same order Add takes and for the same reason.
// A VaultCandidate with no Doc, which is the shape rather than an omission: the
// row itself was scrubbed and sealed when the photograph was hidden, and this
// is only the half that was missed.
func (s *Store) VaultAnalysisLeftBehind(ctx context.Context) ([]VaultCandidate, error) {
	// The four exists clauses rather than `analysisDoc is not null`, which would
	// say the same thing and build the document twice to say it. They are also
	// the cheap half: an archive the sweep has caught up with pays four index
	// probes per hidden photograph and never assembles a document at all.
	rows, err := s.pool.Query(ctx, `
		select a.id::text, a.sha256, a.ext, null::jsonb, `+analysisDoc+`, true
		from assets a
		where a.vault <> ''
		  and (exists (select 1 from asset_descriptions d where d.asset_id = a.id)
		    or exists (select 1 from asset_ocr o where o.asset_id = a.id)
		    or exists (select 1 from asset_tags t where t.asset_id = a.id)
		    or exists (select 1 from asset_embeddings e where e.asset_id = a.id)
		    or exists (select 1 from asset_search x where x.asset_id = a.id))`)
	if err != nil {
		return nil, fmt.Errorf("look for words left behind in the vault: %w", err)
	}
	return scanCandidates(rows)
}

// CommitVaultAnalysis stores the sealed documents the sweep built and takes the
// rows they describe away, in one transaction.
//
// `coalesce` rather than a plain assignment, because a document already in that
// column is the one CommitVault sealed and is by definition the better one: it
// was written from the full set of rows at the moment of hiding, where anything
// this sweep finds beside it could only be a fragment. The same reasoning, and
// the same shape, as CommitVault's `on conflict do nothing`.
//
// The vault-column guard is on both statements rather than trusted from the
// read, because between the read and here somebody may have restored the
// photograph — at which point its caption is in the clear legitimately and
// deleting it would be this sweep destroying the library's own metadata.
func (s *Store) CommitVaultAnalysis(ctx context.Context, items []SealedItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	ids := make([]string, len(items))
	sealed := make([][]byte, len(items))
	for i, it := range items {
		ids[i], sealed[i] = it.AssetID, it.SealedAnalysis
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sweptIDs []string
	rows, err := tx.Query(ctx, `
		update vault_items v
		   set sealed_analysis = coalesce(v.sealed_analysis, k.analysis)
		from unnest($1::text[], $2::bytea[]) as k(id, analysis)
		join assets a on a.id = k.id::uuid and a.vault <> ''
		where v.asset_id = k.id::uuid
		returning v.asset_id::text`, ids, sealed)
	if err != nil {
		return 0, fmt.Errorf("seal the words left behind in the vault: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan a swept vault item: %w", err)
		}
		sweptIDs = append(sweptIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("seal the words left behind in the vault: %w", err)
	}
	if len(sweptIDs) == 0 {
		return 0, nil
	}

	if err := dropAnalysis(ctx, tx, sweptIDs); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the vault analysis sweep: %w", err)
	}
	return len(sweptIDs), nil
}

// VaultSecretRow is the stored vault identity, as columns.
type VaultSecretRow struct {
	PublicKey     []byte
	Salt          []byte
	Time          int32
	Memory        int32
	Threads       int32
	SealedPrivate []byte
}

// VaultSecret reads the vault's identity, or ErrNoVaultSecret if this archive
// has never had one.
func (s *Store) VaultSecret(ctx context.Context) (VaultSecretRow, error) {
	var row VaultSecretRow
	err := s.pool.QueryRow(ctx, `
		select public_key, kdf_salt, kdf_time, kdf_memory, kdf_threads, sealed_private
		from vault_secret where id = 1`).Scan(
		&row.PublicKey, &row.Salt, &row.Time, &row.Memory, &row.Threads, &row.SealedPrivate)
	if errors.Is(err, pgx.ErrNoRows) {
		return VaultSecretRow{}, ErrNoVaultSecret
	}
	if err != nil {
		return VaultSecretRow{}, fmt.Errorf("read the vault secret: %w", err)
	}
	return row, nil
}

// CreateVaultSecret writes the identity, once.
//
// `do nothing` rather than `do update`: creating a vault over an existing one
// would orphan every file already encrypted to the old public key, and there is
// no error message that makes that recoverable. A password *change* is
// SaveVaultSecret, which keeps the keypair.
func (s *Store) CreateVaultSecret(ctx context.Context, row VaultSecretRow) error {
	tag, err := s.pool.Exec(ctx, `
		insert into vault_secret (id, public_key, kdf_salt, kdf_time, kdf_memory, kdf_threads, sealed_private)
		values (1, $1, $2, $3, $4, $5, $6)
		on conflict (id) do nothing`,
		row.PublicKey, row.Salt, row.Time, row.Memory, row.Threads, row.SealedPrivate)
	if err != nil {
		return fmt.Errorf("create the vault: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("db: this archive already has a vault")
	}
	return nil
}

// SaveVaultSecret re-seals the same keypair under new password parameters.
func (s *Store) SaveVaultSecret(ctx context.Context, row VaultSecretRow) error {
	tag, err := s.pool.Exec(ctx, `
		update vault_secret set kdf_salt = $1, kdf_time = $2, kdf_memory = $3,
		                        kdf_threads = $4, sealed_private = $5, updated_at = now()
		where id = 1 and public_key = $6`,
		row.Salt, row.Time, row.Memory, row.Threads, row.SealedPrivate, row.PublicKey)
	if err != nil {
		return fmt.Errorf("change the vault password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoVaultSecret
	}
	return nil
}
