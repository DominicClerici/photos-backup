package db

import (
	"context"
	"fmt"
)

// LibraryStats is what the gallery would draw if you scrolled to the end of it:
// the same predicate the timeline pages over, counted rather than listed.
//
// Deliberately not ArchiveStats. That one counts every stored original,
// including the video half of a Live Photo, a Snapchat caption layer, and
// everything in the trash — right for "how much has this device backed up", and
// wrong on a page whose first card claims to say how many photographs there
// are.
type LibraryStats struct {
	Items  int64 `json:"items"`
	Photos int64 `json:"photos"`
	Videos int64 `json:"videos"`
	// Trashed is counted separately because it is neither in the library nor
	// gone: the bytes are still on the drive for a year.
	Trashed int64 `json:"trashed"`
}

// LibraryStats counts the visible library, and the trash beside it.
func (s *Store) LibraryStats(ctx context.Context) (LibraryStats, error) {
	const query = `
		select count(*) filter (where ` + visibleAssets + `),
		       count(*) filter (where ` + visibleAssets + ` and a.media_kind = 'image'),
		       count(*) filter (where ` + visibleAssets + ` and a.media_kind = 'video'),
		       count(*) filter (where ` + trashedAssets + `)
		from assets a`

	var stats LibraryStats
	err := s.pool.QueryRow(ctx, query).Scan(&stats.Items, &stats.Photos, &stats.Videos, &stats.Trashed)
	if err != nil {
		return LibraryStats{}, fmt.Errorf("count library: %w", err)
	}
	return stats, nil
}

// StoredBytes is what the originals weigh, split the way the storage card
// splits them.
//
// Every row counts, including the components the library hides and the items in
// the trash: this is a question about the disk, and a Live Photo's paired video
// occupies the drive whether or not the gallery draws it as its own tile.
//
// The vault is the one exception. Its originals are on the same drive and their
// bytes are real, but they are encrypted precisely so that nothing without the
// password can say what is in there — and "your hidden photographs come to
// 12GB" is an answer to that question. They land in the status page's
// unaccounted remainder instead, along with the database and the reserved
// blocks.
type StoredBytes struct {
	Photos int64 `json:"photos"`
	Videos int64 `json:"videos"`
}

func (s *Store) StoredBytes(ctx context.Context) (StoredBytes, error) {
	const query = `
		select coalesce(sum(byte_size) filter (where media_kind = 'image'), 0),
		       coalesce(sum(byte_size) filter (where media_kind = 'video'), 0)
		from assets
		where vault = ''`

	var b StoredBytes
	if err := s.pool.QueryRow(ctx, query).Scan(&b.Photos, &b.Videos); err != nil {
		return StoredBytes{}, fmt.Errorf("sum stored bytes: %w", err)
	}
	return b, nil
}

// MediaKinds maps each stored digest to the kind of thing it is.
//
// It exists for one caller: the derivatives on disk are named by digest and
// nothing else, so a thumbnail cannot be charged to photos or to videos without
// this. Returned whole rather than queried per file because the alternative is
// one round trip per thumbnail across a library of tens of thousands.
//
// Vaulted digests are absent, so their renditions stay unattributed for the
// same reason their originals do.
func (s *Store) MediaKinds(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `select distinct sha256, media_kind from assets where vault = ''`)
	if err != nil {
		return nil, fmt.Errorf("list media kinds: %w", err)
	}
	defer rows.Close()

	kinds := make(map[string]string)
	for rows.Next() {
		var sha, kind string
		if err := rows.Scan(&sha, &kind); err != nil {
			return nil, fmt.Errorf("scan media kind: %w", err)
		}
		kinds[sha] = kind
	}
	return kinds, rows.Err()
}

// AssetLabel is the little that a failure report needs to name the thing that
// failed: what it was called, and whether it can still be looked at.
type AssetLabel struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaKind string `json:"media_kind"`
	// Viewable is false for a vaulted asset: the job row outlives the hiding,
	// and a broken transcode is still worth reporting afterwards, but the
	// picture and even its filename are not ours to show. An asset in the trash
	// is viewable — the trash page draws it too.
	Viewable bool `json:"viewable"`
}

// AssetLabels names a set of assets by id. Ids with no row are simply absent:
// a job can outlive the asset it was queued for, which is a fact the caller
// wants rather than an error.
func (s *Store) AssetLabels(ctx context.Context, ids []string) (map[string]AssetLabel, error) {
	if len(ids) == 0 {
		return map[string]AssetLabel{}, nil
	}

	rows, err := s.pool.Query(ctx, `
		select id::text, original_filename, media_kind, vault = ''
		from assets
		where id = any($1::uuid[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("label assets: %w", err)
	}
	defer rows.Close()

	labels := make(map[string]AssetLabel, len(ids))
	for rows.Next() {
		var l AssetLabel
		if err := rows.Scan(&l.ID, &l.Filename, &l.MediaKind, &l.Viewable); err != nil {
			return nil, fmt.Errorf("scan asset label: %w", err)
		}
		// The vault scrubs the filename on the way in, so this is only ever a
		// belt-and-braces against a row that was hidden before it did.
		if !l.Viewable {
			l.Filename = ""
		}
		labels[l.ID] = l
	}
	return labels, rows.Err()
}
