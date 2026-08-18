package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The kinds of thing an import can fail to place. See migration 0008.
const (
	// OrphanSidecar is a sidecar whose media file was not in the export.
	OrphanSidecar = "sidecar"
	// OrphanAlbum is album membership for an item that had no sidecar to carry
	// it to the archive.
	OrphanAlbum = "album"
)

// ImportOrphan is one thing an import read and could not attach.
//
// It is stored rather than applied. An orphan is evidence that something was
// known about a photograph and could not be recorded against it, and the value
// of keeping it is that the export it came from will be deleted long before
// anyone decides what it was worth.
type ImportOrphan struct {
	Source  string
	Kind    string
	Locator string

	// AssetID is set on an album orphan and empty on a sidecar orphan, which by
	// definition has no asset to name.
	AssetID string

	Sidecar json.RawMessage
	Albums  []AlbumRef

	Reason string
}

// Validate checks the parts that decide where a row goes, so a malformed
// request is refused at the edge rather than stored as a row nothing can read.
func (o ImportOrphan) Validate() error {
	switch o.Source {
	case SourceGoogleTakeout, SourcePhotoKit, SourceSnapchat:
	default:
		return fmt.Errorf("unknown import source %q; this archive reads %q, %q and %q",
			o.Source, SourceGoogleTakeout, SourcePhotoKit, SourceSnapchat)
	}

	switch o.Kind {
	case OrphanSidecar:
		if len(o.Sidecar) == 0 {
			return errors.New("a sidecar orphan with no sidecar records nothing")
		}
	case OrphanAlbum:
		if len(o.Albums) == 0 {
			return errors.New("an album orphan with no albums records nothing")
		}
	default:
		return fmt.Errorf("unknown orphan kind %q; this archive records %q and %q",
			o.Kind, OrphanSidecar, OrphanAlbum)
	}

	if strings.TrimSpace(o.Locator) == "" {
		return errors.New("locator is required; it is what makes a re-import update this row rather than add one")
	}
	return nil
}

// RecordImportOrphan stores one, or refreshes the one already there.
//
// Upserted on (source, kind, locator) because re-running an import is the
// ordinary way to recover a half-finished one, and it re-reads every sidecar it
// could not place last time. last_seen moving is the useful signal: an orphan
// whose timestamp stops advancing is one a later delivery resolved.
func (s *Store) RecordImportOrphan(ctx context.Context, o ImportOrphan) error {
	if err := o.Validate(); err != nil {
		return err
	}

	const upsert = `
		insert into import_orphans (source, kind, locator, asset_id, sidecar, albums, reason)
		values ($1, $2, $3, nullif($4, '')::uuid, $5::jsonb, $6::jsonb, $7)
		on conflict (source, kind, locator) do update set
			asset_id  = coalesce(excluded.asset_id, import_orphans.asset_id),
			sidecar   = coalesce(excluded.sidecar, import_orphans.sidecar),
			albums    = coalesce(excluded.albums, import_orphans.albums),
			reason    = excluded.reason,
			last_seen = now()`

	var sidecar, albums any
	if len(o.Sidecar) > 0 {
		sidecar = []byte(o.Sidecar)
	}
	if len(o.Albums) > 0 {
		encoded, err := json.Marshal(o.Albums)
		if err != nil {
			return fmt.Errorf("encode albums for %s: %w", o.Locator, err)
		}
		albums = encoded
	}

	if _, err := s.pool.Exec(ctx, upsert,
		o.Source, o.Kind, o.Locator, o.AssetID, sidecar, albums, o.Reason); err != nil {
		return fmt.Errorf("record import orphan %s: %w", o.Locator, err)
	}
	return nil
}

// ImportOrphanCounts is how many of each kind are outstanding, for the import
// summary and for anything that later wants to say the archive has unresolved
// evidence sitting in it.
func (s *Store) ImportOrphanCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `select kind, count(*) from import_orphans group by kind`)
	if err != nil {
		return nil, fmt.Errorf("count import orphans: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		counts[kind] = n
	}
	return counts, rows.Err()
}
