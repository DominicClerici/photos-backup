package db

import (
	"context"
	"fmt"
)

// ApplyMetadata records what the worker read off an original and marks the
// asset ready to display.
//
// captured_at is never touched. The phone's value and the file's value are kept
// side by side so a disagreement stays visible, and the timeline's generated
// sort_time column prefers the file's — which is what puts an old photo
// imported into Photos last week in the right place rather than at the top.
func (s *Store) ApplyMetadata(ctx context.Context, assetID string, m Metadata) error {
	const update = `
		update assets set
			width = $2, height = $3, orientation = $4, duration_seconds = $5,
			camera_make = nullif($6, ''), camera_model = nullif($7, ''), lens = nullif($8, ''),
			gps_lat = $9, gps_lon = $10,
			exif_captured_at = $11, exif_offset_minutes = $12,
			derived_state = 'ready'
		where id = $1::uuid`

	tag, err := s.pool.Exec(ctx, update, assetID,
		m.Width, m.Height, m.Orientation, m.DurationSeconds,
		m.CameraMake, m.CameraModel, m.Lens,
		m.GPSLat, m.GPSLon,
		m.ExifCapturedAt, m.ExifOffsetMinutes)
	if err != nil {
		return fmt.Errorf("apply metadata to %s: %w", assetID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDerivedState records the outcome of the metadata job when it did not
// produce metadata — that is, when it failed for good.
func (s *Store) SetDerivedState(ctx context.Context, assetID, state string) error {
	if _, err := s.pool.Exec(ctx,
		`update assets set derived_state = $2 where id = $1::uuid`, assetID, state); err != nil {
		return fmt.Errorf("set derived state of %s: %w", assetID, err)
	}
	return nil
}

// SetPlaybackState records progress on the video transcode, which the viewer
// reads to decide between showing a player and showing "still converting".
func (s *Store) SetPlaybackState(ctx context.Context, assetID, state string) error {
	if _, err := s.pool.Exec(ctx,
		`update assets set playback_state = $2 where id = $1::uuid`, assetID, state); err != nil {
		return fmt.Errorf("set playback state of %s: %w", assetID, err)
	}
	return nil
}
