package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// eachAssetBatch is how many rows are read per round trip. Large enough that a
// 400,000-asset library is a few hundred queries, small enough that the slice
// is nothing.
const eachAssetBatch = 1000

// EachAsset streams every archived asset, oldest id first.
//
// Paged on a keyset rather than held open as a cursor on purpose: the callers
// are verify and export, which do slow filesystem work between rows — hashing
// a 550MB blob, or linking a file. A cursor would pin a pooled connection open
// for the length of a full-archive pass, which on a 6TB drive is hours.
func (s *Store) EachAsset(ctx context.Context, fn func(Asset) error) error {
	after := ""
	for {
		const query = `select ` + assetColumns + `
			from assets
			where ($1 = '' or id > $1::uuid)
			order by id
			limit $2`

		rows, err := s.pool.Query(ctx, query, after, eachAssetBatch)
		if err != nil {
			return fmt.Errorf("scan assets: %w", err)
		}

		batch := make([]Asset, 0, eachAssetBatch)
		for rows.Next() {
			a, err := scanAsset(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("scan asset row: %w", err)
			}
			batch = append(batch, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read assets: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}

		// The connection is back in the pool before fn runs.
		for _, a := range batch {
			if err := fn(a); err != nil {
				return err
			}
		}
		after = batch[len(batch)-1].ID
	}
}

// AssetBySHA256 looks an asset up by content, which is how verify and reindex
// address one: they are working from the blob tree and the manifest, where the
// digest is the only identifier that exists.
func (s *Store) AssetBySHA256(ctx context.Context, sha string) (Asset, error) {
	row := s.pool.QueryRow(ctx, `select `+assetColumns+` from assets where sha256 = $1`, sha)
	a, err := scanAsset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, fmt.Errorf("load asset by sha256: %w", err)
	}
	return a, nil
}

// Counts is the summary verify and reindex print before doing anything, so a
// run that is about to do nothing says so immediately.
type Counts struct {
	Assets        int64
	Bytes         int64
	DerivedFailed int64
	Videos        int64
}

func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx, `
		select count(*),
		       coalesce(sum(byte_size), 0),
		       count(*) filter (where derived_state = 'failed'),
		       count(*) filter (where media_kind = 'video')
		from assets`).Scan(&c.Assets, &c.Bytes, &c.DerivedFailed, &c.Videos)
	if err != nil {
		return Counts{}, fmt.Errorf("count assets: %w", err)
	}
	return c, nil
}

// DeviceMapping is one (device, local id) association, for a rebuild that has
// to restore them from the manifest.
type DeviceMapping struct {
	DeviceID   string
	LocalID    string
	AssetID    string
	ModifiedAt *time.Time
}

// RecordDeviceMapping restores a single mapping without touching the asset row.
//
// Reindex needs this separately from RecordAsset: the manifest records who
// delivered each blob, and replaying those mappings is what stops a rebuilt
// database from asking the phone to re-hash its entire library on the next run.
func (s *Store) RecordDeviceMapping(ctx context.Context, m DeviceMapping) error {
	if m.DeviceID == "" || m.LocalID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		insert into device_assets (device_id, local_id, asset_id, modified_at)
		values ($1, $2, $3, $4)
		on conflict (device_id, local_id)
		do update set asset_id = excluded.asset_id, modified_at = excluded.modified_at`,
		m.DeviceID, m.LocalID, m.AssetID, m.ModifiedAt)
	if err != nil {
		return fmt.Errorf("record device mapping: %w", err)
	}
	return nil
}
