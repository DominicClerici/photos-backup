package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/photokit"
	"github.com/dominicclerici/photos-backup/server/internal/takeout"
)

// ImportMetadata is what an importer learned about an asset from somewhere
// other than the asset. For a Google Takeout that is the sidecar JSON beside
// each file, plus the directory it was found in.
//
// It is normalized rather than passed through because the columns it feeds are
// shared with the phone and with the metadata worker, and none of them should
// have to know what Google calls a caption. Raw is carried alongside so that
// normalizing is not the same as discarding.
type ImportMetadata struct {
	// Source names the importer, e.g. "google-takeout". It namespaces albums
	// and records where the rest of this came from.
	Source string
	// Raw is the sidecar verbatim, stored so that a field nobody modelled today
	// is still there to be modelled tomorrow. The export it came from is
	// usually deleted the week after the import.
	Raw json.RawMessage

	Description string
	Favorite    bool
	Archived    bool

	// TakenAt is the source's capture time, which for anything with intact EXIF
	// is redundant and for a screenshot is the only one that exists.
	TakenAt *time.Time
	// GPSLat and GPSLon are the source's coordinates, same story.
	GPSLat *float64
	GPSLon *float64

	People []string
	Albums []AlbumRef
	// Subtypes are what the source called this asset — "screenshot",
	// "livePhoto", "panorama". A Takeout knows none; PhotoKit knows them all
	// and nothing ever asked it.
	Subtypes []string
}

// AlbumRef is an album an asset belongs to, as the source named it.
type AlbumRef struct {
	Title       string
	Description string
}

// The sources whose sidecars this archive can read.
//
// Two of them, and they are not the same kind of thing: one is an export of a
// service, the other is the device the photographs came off. What makes them
// one mechanism is that both know things the file does not — a heart, an album,
// a caption — and neither can be asked again once the export is deleted or the
// phone is wiped.
const (
	SourceGoogleTakeout = "google-takeout"
	SourcePhotoKit      = "ios-photokit"
)

// ImportMetadataFrom interprets a source's own sidecar.
//
// It lives here, beside the type it produces, rather than in the handler that
// first receives one, because a rebuild has to do exactly the same thing: the
// manifest stores sidecars raw so a reindex re-reads them with today's parser
// instead of replaying an older parser's conclusions. Two callers, one meaning.
//
// An unknown source is refused rather than stored uninterpreted. Keeping the
// raw JSON of a format nothing can read would look like the data had been
// captured while holding nothing anything could show.
func ImportMetadataFrom(source string, sidecar []byte, albums []AlbumRef) (ImportMetadata, error) {
	if len(sidecar) == 0 {
		return ImportMetadata{}, errors.New("sidecar is required")
	}

	switch source {
	case SourceGoogleTakeout:
		return fromTakeout(sidecar, albums)
	case SourcePhotoKit:
		return fromPhotoKit(sidecar, albums)
	default:
		return ImportMetadata{}, fmt.Errorf(
			"unknown import source %q; this archive reads %q and %q",
			source, SourceGoogleTakeout, SourcePhotoKit)
	}
}

func fromTakeout(sidecar []byte, albums []AlbumRef) (ImportMetadata, error) {
	parsed, err := takeout.Normalize(sidecar)
	if err != nil {
		return ImportMetadata{}, err
	}

	return ImportMetadata{
		Source:      SourceGoogleTakeout,
		Raw:         parsed.Raw,
		Description: parsed.Description,
		Favorite:    parsed.Favorite,
		// Trashed items are recorded as archived rather than as a state of
		// their own. The archive does not delete, so "the source had this in
		// the bin" is the same kind of fact as "the source had this out of the
		// way", and neither is this archive's business to act on.
		Archived: parsed.Archived || parsed.Trashed,
		TakenAt:  parsed.TakenAt,
		GPSLat:   parsed.GPSLat,
		GPSLon:   parsed.GPSLon,
		People:   parsed.People,
		Albums:   albums,
	}, nil
}

// fromPhotoKit reads the phone's own description of an asset it has uploaded.
//
// It carries no caption and no capture time: the app sends the capture time as
// an upload header where it belongs, and iOS has nowhere to type a caption.
// What it has that nothing else does is the heart, the albums, Apple's own
// classification of the shot, and — since the phone learned to read PHAsset
// directly rather than through expo-media-library — the Hidden album, the
// burst, the source and whether the shot was ever edited. Only the first four
// and the hiding become columns; the rest is kept whole in Raw.
func fromPhotoKit(sidecar []byte, albums []AlbumRef) (ImportMetadata, error) {
	parsed, err := photokit.Normalize(sidecar)
	if err != nil {
		return ImportMetadata{}, err
	}

	return ImportMetadata{
		Source:   SourcePhotoKit,
		Raw:      parsed.Raw,
		Favorite: parsed.Favorite,
		// The Hidden album lands on the same flag a Takeout's archived and
		// trashed items do. Putting a photo in Hidden is a person saying "not
		// in the roll, but keep it", which is the same kind of fact as Google's
		// "this was out of the way" — and this archive records that kind of
		// fact rather than acting on it, so an asset the phone hid is still
		// archived, still in the timeline, and flagged for whoever wants to
		// filter on it.
		Archived: parsed.Hidden,
		GPSLat:   parsed.GPSLat,
		GPSLon:   parsed.GPSLon,
		Subtypes: parsed.Subtypes,
		Albums:   albums,
	}, nil
}

// ApplyImportMetadata merges a sidecar into an asset.
//
// The rule throughout is that an import fills gaps and never empties them. It
// is a merge, not an assignment: the same photo can be described by the phone
// that took it and by an export of the cloud service it was uploaded to, the
// two arrive in an order nobody controls, and an export that mentions no
// caption is not asserting that there is no caption.
//
// That also makes a re-import idempotent, which matters more than it sounds —
// a Takeout is delivered as a pile of zips and the natural way to recover from
// a half-finished import is to run the whole thing again.
func (s *Store) ApplyImportMetadata(ctx context.Context, assetID string, m ImportMetadata) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const update = `
		update assets set
			description     = coalesce(nullif($2, ''), description),
			favorite        = favorite or $3,
			archived        = archived or $4,
			import_source   = coalesce(nullif($5, ''), import_source),
			import_metadata = coalesce($6::jsonb, import_metadata),
			import_gps_lat  = coalesce($7, import_gps_lat),
			import_gps_lon  = coalesce($8, import_gps_lon),
			-- Merged rather than replaced, and never emptied: a source that
			-- names no subtypes is not asserting the photo has none.
			subtypes = (
				select coalesce(array_agg(distinct value order by value), '{}')
				from unnest(subtypes || $10::text[]) as value
			),
			-- The canonical columns, filled only where nothing else has. The
			-- metadata worker owns gps_lat/gps_lon and re-applies this same
			-- fallback, so these two stay correct whichever job ran last.
			gps_lat         = coalesce(gps_lat, $7),
			gps_lon         = coalesce(gps_lon, $8),
			captured_at     = coalesce(captured_at, $9)
		where id = $1::uuid`

	var raw any
	if len(m.Raw) > 0 {
		raw = []byte(m.Raw)
	}
	subtypes := m.Subtypes
	if subtypes == nil {
		subtypes = []string{}
	}
	tag, err := tx.Exec(ctx, update, assetID,
		m.Description, m.Favorite, m.Archived, m.Source, raw,
		m.GPSLat, m.GPSLon, m.TakenAt, subtypes)
	if err != nil {
		return fmt.Errorf("apply import metadata to %s: %w", assetID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if err := applyPeople(ctx, tx, assetID, m.People); err != nil {
		return err
	}
	if err := applyAlbums(ctx, tx, assetID, m.Source, m.Albums); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func applyPeople(ctx context.Context, tx pgx.Tx, assetID string, names []string) error {
	names = dedupe(names)
	if len(names) == 0 {
		return nil
	}
	const insert = `
		insert into asset_people (asset_id, name)
		select $1::uuid, n from unnest($2::text[]) as n
		on conflict do nothing`
	if _, err := tx.Exec(ctx, insert, assetID, names); err != nil {
		return fmt.Errorf("record people on %s: %w", assetID, err)
	}
	return nil
}

// applyAlbums records membership, creating albums it has not seen.
//
// Albums are keyed by (source, title) rather than by id, because the export has
// no album ids — a directory name is the whole identity — and because that is
// what makes running the import twice produce one album rather than two.
func applyAlbums(ctx context.Context, tx pgx.Tx, assetID, source string, albums []AlbumRef) error {
	for _, album := range albums {
		if album.Title == "" {
			continue
		}
		var albumID string
		const upsert = `
			insert into albums (source, title, description)
			values ($1, $2, $3)
			on conflict (source, title) do update
				set description = coalesce(nullif(excluded.description, ''), albums.description)
			returning id::text`
		if err := tx.QueryRow(ctx, upsert, source, album.Title, album.Description).Scan(&albumID); err != nil {
			return fmt.Errorf("record album %q: %w", album.Title, err)
		}

		const member = `
			insert into album_assets (album_id, asset_id) values ($1::uuid, $2::uuid)
			on conflict do nothing`
		if _, err := tx.Exec(ctx, member, albumID, assetID); err != nil {
			return fmt.Errorf("add %s to album %q: %w", assetID, album.Title, err)
		}
	}
	return nil
}

// AssetExtras are the parts of an asset that live in their own tables, fetched
// only for a single open asset rather than for every tile on a page.
type AssetExtras struct {
	Albums []string
	People []string
}

func (s *Store) AssetExtras(ctx context.Context, assetID string) (AssetExtras, error) {
	var out AssetExtras

	const albums = `
		select a.title from album_assets m
		join albums a on a.id = m.album_id
		where m.asset_id = $1::uuid
		order by a.title`
	if err := collect(ctx, s, albums, assetID, &out.Albums); err != nil {
		return out, fmt.Errorf("load albums for %s: %w", assetID, err)
	}

	const people = `select name from asset_people where asset_id = $1::uuid order by name`
	if err := collect(ctx, s, people, assetID, &out.People); err != nil {
		return out, fmt.Errorf("load people for %s: %w", assetID, err)
	}
	return out, nil
}

func collect(ctx context.Context, s *Store, query, arg string, into *[]string) error {
	rows, err := s.pool.Query(ctx, query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		*into = append(*into, value)
	}
	return rows.Err()
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := values[:0:0]
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
