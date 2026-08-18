package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The write half of albums: making one, and moving photographs in and out of it.
//
// Everything here is deliberately small, because an album is a small thing. It
// is a title and a membership list — no ordering of its own, no cover somebody
// chose, no per-album settings — and every question about what is *in* one is
// answered by the timeline with an album filter, which already exists. What
// this file adds is the three writes that were missing: create, add, remove.
//
// Albums made here carry an empty source, which is the same field an import
// namespaces its albums with. Empty means "made in the gallery", and it is what
// keeps a person's "Favorites" and a Takeout's "Favorites" from being the same
// row. See migration 0006 for the key and 0013 for its scope.

// GallerySource is the source an album made by hand carries: none. Written down
// rather than inlined because the create path and the uniqueness check have to
// agree about what "an album with this name already exists" is asking of.
const GallerySource = ""

// ErrDuplicateAlbum means the name is taken by an album that still exists.
//
// A refusal rather than a second album of the same name, and the constraint is
// the one that decides it — checking first and inserting afterwards would be
// the same answer with a race in it. See migration 0013 for what "still exists"
// covers and, in particular, for the one case where the name is taken by
// something the person asking cannot see.
var ErrDuplicateAlbum = errors.New("db: an album with that name already exists")

// NewAlbum is an album somebody has just described in a dialog.
type NewAlbum struct {
	Title       string
	Description string
	// Vault is empty for the library, or the bucket the album is being made
	// inside. An album created in the Archive is an archived album from the
	// moment it exists rather than one that gets moved there afterwards — there
	// is nothing in it to move.
	Vault string
}

// CreateAlbum makes an empty album and hands back enough of it to draw.
func (s *Store) CreateAlbum(ctx context.Context, spec NewAlbum) (Album, error) {
	if spec.Vault != "" && !ValidBucket(spec.Vault) {
		return Album{}, fmt.Errorf("%w: %q", ErrBadBucket, spec.Vault)
	}

	// vaulted_at is set with the row rather than left null, because it is what
	// the bucket's own page orders by and a null there would sort an album
	// somebody just made to the wrong end of it.
	var album Album
	err := s.pool.QueryRow(ctx, `
		insert into albums (source, title, description, vault, vaulted_at)
		values ($1, $2, $3, $4, case when $4 = '' then null else now() end)
		returning id::text, source, title, description`,
		GallerySource, spec.Title, spec.Description, spec.Vault).
		Scan(&album.ID, &album.Source, &album.Title, &album.Description)
	if err != nil {
		if isDuplicate(err) {
			return Album{}, ErrDuplicateAlbum
		}
		return Album{}, fmt.Errorf("create album %q: %w", spec.Title, err)
	}
	return album, nil
}

// AlbumHome is an album's row and which half of the archive it is in: "" for
// the library, or the bucket it was hidden into.
//
// Every write that names an album goes through here first. It is the check that
// an id from a client is a real, live album at all, and it is what tells the two
// membership mechanisms apart — see ErrWrongPlace. The title comes back with it
// because the vault's membership is written into sealed documents that carry the
// title alongside the id, so the caller needs both.
func (s *Store) AlbumHome(ctx context.Context, id string) (Album, string, error) {
	var album Album
	var bucket string
	err := s.pool.QueryRow(ctx, `
		select id::text, source, title, description, vault
		from albums where id = $1::uuid and deleted_at is null`, id).
		Scan(&album.ID, &album.Source, &album.Title, &album.Description, &bucket)
	if errors.Is(err, pgx.ErrNoRows) {
		return Album{}, "", ErrNotFound
	}
	if err != nil {
		return Album{}, "", fmt.Errorf("look up album %s: %w", id, err)
	}
	return album, bucket, nil
}

// AddToAlbum puts a selection of the library into an album.
//
// The count is of rows actually inserted, so adding a selection that is already
// in the album reports nothing added rather than claiming to have done it
// again. The client says so — "already in Iceland 2025" — and that is a better
// sentence than a toast that lies by a number.
//
// Components are not included and are not missing: the selection resolves
// through visibleAssets, which excludes them, and album membership has never
// been about them. A Live Photo's motion is part of the still, and the still is
// what is in the album.
func (s *Store) AddToAlbum(ctx context.Context, albumID string, sel Selection) (int, error) {
	sel.Filter.Trash = false

	pick, args, err := sel.pick(2)
	if err != nil {
		return 0, err
	}
	args = append([]any{albumID}, args...)

	var added int
	if err := s.pool.QueryRow(ctx, `
		with sel as (`+pick+`),
		done as (
			insert into album_assets (album_id, asset_id)
			select $1::uuid, sel.id from sel
			on conflict do nothing
			returning asset_id
		)
		select count(*)::int from done`, args...).Scan(&added); err != nil {
		return 0, fmt.Errorf("add a selection to album %s: %w", albumID, err)
	}
	return added, nil
}

// RemoveFromAlbum takes a selection back out of one.
//
// It removes the grouping and nothing else: every photograph stays exactly
// where it was in the library, in every other album, and in the timeline. That
// is why it is not a delete and does not go anywhere near the trash — and why
// the control that runs it is armed rather than confirmed, in the same way the
// two readings of "delete this album" are told apart in trash.go.
//
// The selection is usually positions inside the album itself, which resolve
// through the album's own filter — so "remove the last forty" means the last
// forty of this album, not of the library.
func (s *Store) RemoveFromAlbum(ctx context.Context, albumID string, sel Selection) (int, error) {
	sel.Filter.Trash = false

	pick, args, err := sel.pick(2)
	if err != nil {
		return 0, err
	}
	args = append([]any{albumID}, args...)

	var removed int
	if err := s.pool.QueryRow(ctx, `
		with sel as (`+pick+`),
		done as (
			delete from album_assets m
			where m.album_id = $1::uuid and m.asset_id in (select id from sel)
			returning m.asset_id
		)
		select count(*)::int from done`, args...).Scan(&removed); err != nil {
		return 0, fmt.Errorf("remove a selection from album %s: %w", albumID, err)
	}
	return removed, nil
}

// AlbumIDsOf is which albums hold one photograph.
//
// Ids rather than the titles AssetExtras reads, because this answers a
// different question. The viewer's panel is telling somebody what an album is
// called; this is deciding which rows of a menu get a tick, and a title is not
// a name a menu can match on when two imports each contributed a "Favorites".
func (s *Store) AlbumIDsOf(ctx context.Context, assetID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		select m.album_id::text from album_assets m
		join albums al on al.id = m.album_id
		where m.asset_id = $1::uuid and al.deleted_at is null
		order by al.title`, assetID)
	if err != nil {
		return nil, fmt.Errorf("read the albums of %s: %w", assetID, err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan album membership: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// isDuplicate reports the one constraint violation this package turns into an
// answer rather than a failure.
func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
