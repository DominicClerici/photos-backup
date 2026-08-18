package db

import (
	"context"
	"fmt"
	"time"
)

// DeviceStats is what one paired device has put into the archive. It answers
// the phone's question — how much of my library is safe — rather than the
// archive's.
//
// Bytes is summed per mapping, so the same photo saved twice in one library
// counts twice. That is deliberate: the phone has two items and considers both
// backed up, and a number that disagreed with its own queue would read as a
// bug. Content-addressed storage means the archive spends less disk than this;
// ArchiveStats is where the true figure lives.
type DeviceStats struct {
	Archived int64 `json:"archived"`
	Bytes    int64 `json:"bytes"`
	Photos   int64 `json:"photos"`
	Videos   int64 `json:"videos"`
	// LastUploadAt is when this device most recently got something new into the
	// archive. Null for a device that has delivered nothing.
	LastUploadAt *time.Time `json:"last_upload_at,omitempty"`
}

// ArchiveStats is the whole archive, every device included, counted once per
// stored original.
type ArchiveStats struct {
	Assets int64 `json:"assets"`
	Bytes  int64 `json:"bytes"`
	Photos int64 `json:"photos"`
	Videos int64 `json:"videos"`
}

// DeviceStats counts what a device has delivered.
//
// LastUploadAt reads device_assets.first_seen rather than assets.uploaded_at,
// because they answer different questions. A photo whose bytes were already
// archived by another device is still something this phone backed up today, and
// uploaded_at would report whenever the other device happened to send it —
// possibly years earlier. first_seen is when this mapping came into being, which
// is what "last backup" means on a phone screen.
func (s *Store) DeviceStats(ctx context.Context, deviceID string) (DeviceStats, error) {
	var stats DeviceStats
	if deviceID == "" {
		return stats, nil
	}

	const query = `
		select count(*),
		       coalesce(sum(a.byte_size), 0),
		       count(*) filter (where a.media_kind = 'image'),
		       count(*) filter (where a.media_kind = 'video'),
		       max(d.first_seen)
		from device_assets d
		join assets a on a.id = d.asset_id
		where d.device_id = $1`

	err := s.pool.QueryRow(ctx, query, deviceID).Scan(
		&stats.Archived, &stats.Bytes, &stats.Photos, &stats.Videos, &stats.LastUploadAt)
	if err != nil {
		return DeviceStats{}, fmt.Errorf("count device assets: %w", err)
	}
	return stats, nil
}

// ArchiveStats counts every stored original once.
//
// A sequential aggregate over assets, which is right for a library measured in
// tens of thousands of rows and read once when the app launches. If it ever
// needs to be cheaper, the answer is a summary row maintained by the upload
// path, not an index — there is no predicate here to index on.
func (s *Store) ArchiveStats(ctx context.Context) (ArchiveStats, error) {
	const query = `
		select count(*),
		       coalesce(sum(byte_size), 0),
		       count(*) filter (where media_kind = 'image'),
		       count(*) filter (where media_kind = 'video')
		from assets`

	var stats ArchiveStats
	err := s.pool.QueryRow(ctx, query).Scan(&stats.Assets, &stats.Bytes, &stats.Photos, &stats.Videos)
	if err != nil {
		return ArchiveStats{}, fmt.Errorf("count archive assets: %w", err)
	}
	return stats, nil
}
