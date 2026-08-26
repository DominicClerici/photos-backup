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
//
// The coordinates are the one field with a third source. An import sidecar can
// carry them for a file whose own EXIF has none — a screenshot, or anything a
// messaging app stripped — and this job runs after the import wrote them and
// would otherwise overwrite them with the null it read. Falling back rather
// than merging keeps the precedence honest in both directions: the file's own
// answer wins whenever it has one, and the sidecar fills the gap, on this run
// and on every re-run.
// The caption is the one field here that a second source also writes, so it
// follows the coordinates' rule rather than overwriting: exif_description is
// what the file said, and description keeps whatever it already had — a caption
// typed into Google Photos outranks a camera's "Screenshot".
func (s *Store) ApplyMetadata(ctx context.Context, assetID string, m Metadata) error {
	const update = `
		update assets set
			width = $2, height = $3, orientation = $4, duration_seconds = $5,
			camera_make = nullif($6, ''), camera_model = nullif($7, ''), lens = nullif($8, ''),
			gps_lat = coalesce($9, import_gps_lat), gps_lon = coalesce($10, import_gps_lon),
			exif_captured_at = $11, exif_offset_minutes = $12,

			exif_metadata = $13::jsonb,
			gps_altitude = $14, gps_direction = $15, gps_accuracy = $16, gps_at = $17,
			iso = $18, f_number = $19, exposure_seconds = $20,
			focal_length = $21, focal_length_35 = $22, flash = $23,
			exif_description = nullif($24, ''),
			description = coalesce(description, nullif($24, '')),
			color_profile = nullif($25, ''), capture_type = $26,
			video_codec = nullif($27, ''), frame_rate = $28, bitrate = $29,
			audio_codec = nullif($30, ''), audio_channels = $31,
			faces = $32::jsonb,

			derived_state = 'ready'
		where id = $1::uuid`

	var raw, faces any
	if len(m.Raw) > 0 {
		raw = []byte(m.Raw)
	}
	if len(m.Faces) > 0 {
		faces = []byte(m.Faces)
	}

	tag, err := s.pool.Exec(ctx, update, assetID,
		m.Width, m.Height, m.Orientation, m.DurationSeconds,
		m.CameraMake, m.CameraModel, m.Lens,
		m.GPSLat, m.GPSLon,
		m.ExifCapturedAt, m.ExifOffsetMinutes,
		raw,
		m.GPSAltitude, m.GPSDirection, m.GPSAccuracy, m.GPSAt,
		m.ISO, m.FNumber, m.ExposureSeconds,
		m.FocalLength, m.FocalLength35, m.Flash,
		m.Description,
		m.ColorProfile, m.CaptureType,
		m.VideoCodec, m.FrameRate, m.Bitrate,
		m.AudioCodec, m.AudioChannels,
		faces)
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

// SetLiveState records progress on a paired video's 256px motion rendition,
// which the grid reads to decide whether a still can be hovered to life.
//
// It refuses to promote an asset that is not half of a Live Photo, so a
// mis-queued job cannot make an ordinary video claim motion it does not have.
// Both kinds of pairing count, since an imported video is only ever the
// resolved kind — see IsLivePair.
func (s *Store) SetLiveState(ctx context.Context, assetID, state string) error {
	if _, err := s.pool.Exec(ctx,
		`update assets set live_state = $2
		 where id = $1::uuid
		   and (live_parent_local_id <> '' or live_parent_asset_id is not null)`, assetID, state); err != nil {
		return fmt.Errorf("set live state of %s: %w", assetID, err)
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

// StalledPlayback is a video whose row promises a playback rendition that
// nothing is going to deliver.
type StalledPlayback struct {
	AssetID string
	SHA256  string
	// HasOverlay reports whether the asset carries a caption layer, and so is
	// owed a second rendition without it.
	HasOverlay bool
}

// StalledPlaybackAssets finds videos left saying 'pending' with no live
// playback job behind the promise: the job ran to done or gave up, or there is
// no job row at all.
//
// The inverse of the question verify used to ask about renditions, and the one
// nothing could answer. "Marked ready but the file is gone" is visible from the
// filesystem; this is only visible from the queue, because a row that says
// pending looks exactly like a video uploaded a second ago until you know that
// the work behind it is finished. A stalled row is not repaired by waiting —
// the viewer shows "preparing a version this browser can play" over it forever.
func (s *Store) StalledPlaybackAssets(ctx context.Context) ([]StalledPlayback, error) {
	const query = `
		select a.id::text, a.sha256, a.overlay_asset_id is not null
		from assets a
		left join jobs j on j.asset_id = a.id and j.kind = 'playback'
		where a.media_kind = 'video'
		  and a.playback_state = 'pending'
		  and a.vault = ''
		  and (j.id is null or j.state in ('done', 'failed'))
		order by a.id`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list stalled playback assets: %w", err)
	}
	defer rows.Close()

	var stalled []StalledPlayback
	for rows.Next() {
		var v StalledPlayback
		if err := rows.Scan(&v.AssetID, &v.SHA256, &v.HasOverlay); err != nil {
			return nil, fmt.Errorf("scan stalled playback asset: %w", err)
		}
		stalled = append(stalled, v)
	}
	return stalled, rows.Err()
}
