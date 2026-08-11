// Package db owns the Postgres schema and every query against it.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a lookup by id matches no row.
var ErrNotFound = errors.New("db: asset not found")

// Asset is one archived original. ID and UploadedAt are assigned by the
// database; the upload supplies the rest, and the derivative worker fills in
// everything from MediaKind's neighbours downward.
type Asset struct {
	ID               string
	SHA256           string
	MD5              string
	ByteSize         int64
	OriginalFilename string
	Ext              string
	ContentType      string
	MediaKind        string
	CapturedAt       *time.Time
	UploadedAt       time.Time
	DeviceID         string
	LocalID          string
	// ModifiedAt is the local asset's modification time on the phone. It
	// describes the delivery rather than the content, so it is stored on the
	// device_assets mapping and never on the asset row.
	ModifiedAt *time.Time

	// Everything below is written by the metadata job, read off the original
	// itself rather than reported by the phone.
	Metadata

	// LiveParentLocalID is the local id of the still this asset is the paired
	// video of, exactly as the phone declared it on upload. Empty on everything
	// that is not half of a Live Photo, and the only thing that identifies one:
	// it is known the moment the bytes arrive, before the still it belongs to
	// necessarily exists.
	LiveParentLocalID string
	// LiveParentAssetID is the archived still it resolved to, or nil while that
	// still has not been uploaded yet.
	LiveParentAssetID *string
	// LiveState tracks the 256px motion rendition. "none" unless this is a
	// paired video.
	LiveState string

	// SortTime is the generated column the timeline orders on:
	// exif capture time, else the phone's, else arrival.
	SortTime time.Time
	// DerivedState tracks the metadata job — pending, ready, or failed. The
	// gallery renders a tile for all three, so an asset is never invisible.
	DerivedState string
	// PlaybackState tracks the transcode. "none" for photos, which have no
	// playback rendition to build.
	PlaybackState string
}

// IsLivePair reports whether this asset is a Live Photo's paired video. It
// reads the declaration rather than the resolution on purpose: an asset is one
// half of a Live Photo from the moment it lands, whether or not the other half
// is here yet.
func (a Asset) IsLivePair() bool { return a.LiveParentLocalID != "" }

// Metadata is what the worker reads out of an original. Every field is optional
// because plenty of real files carry none of it: a screenshot has no camera, a
// messaging-app image has had its EXIF stripped.
type Metadata struct {
	Width             *int
	Height            *int
	Orientation       *int
	DurationSeconds   *float64
	CameraMake        string
	CameraModel       string
	Lens              string
	GPSLat            *float64
	GPSLon            *float64
	ExifCapturedAt    *time.Time
	ExifOffsetMinutes *int
}

// Derivative states, shared by DerivedState and PlaybackState.
const (
	DerivedPending = "pending"
	DerivedReady   = "ready"
	DerivedFailed  = "failed"
	// PlaybackNone marks an asset that will never have a playback rendition.
	PlaybackNone = "none"
)

const (
	MediaImage = "image"
	MediaVideo = "video"
)

// LocalRef identifies a local asset the phone is asking about, with the
// modification time it currently reports.
type LocalRef struct {
	LocalID    string
	ModifiedAt *time.Time
}

// ContentKey addresses content by declared digest and length. Both halves are
// required: size alone is far too weak, and md5 alone would let a collision
// stand in for bytes the archive has never seen.
type ContentKey struct {
	MD5      string
	ByteSize int64
}

// ContentMatch reports what a ContentKey resolved to. Matches is the number of
// assets that matched, so an ambiguous key can be rejected instead of guessed.
type ContentMatch struct {
	AssetID string
	Matches int
}

// Mapping is one (local id -> asset) association to record for a device.
type Mapping struct {
	LocalID    string
	AssetID    string
	ModifiedAt *time.Time
}

type Store struct {
	pool *pgxpool.Pool
	url  string
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool, url: url}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool exposes the connection pool so the job queue can share it rather than
// opening a second one against the same database.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate brings the schema up to date. goose needs a database/sql handle, so
// this borrows one through the pgx stdlib adapter rather than holding it open.
func (s *Store) Migrate() error {
	cfg, err := pgx.ParseConfig(s.url)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()

	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

const assetColumns = `id, sha256, md5, byte_size, original_filename, ext,
	content_type, media_kind, captured_at, uploaded_at, device_id, local_id,
	width, height, orientation, duration_seconds,
	coalesce(camera_make, ''), coalesce(camera_model, ''), coalesce(lens, ''),
	gps_lat, gps_lon, exif_captured_at, exif_offset_minutes,
	live_parent_local_id, live_parent_asset_id::text, live_state,
	sort_time, derived_state, playback_state`

// RecordAsset stores an asset and the mapping from the local asset that
// delivered it, in one transaction. It returns the existing row's id when this
// content is already archived; inserted reports whether an asset row was
// created.
//
// The mapping is written even when the content is a duplicate. That is the
// whole point: without it, a local id whose bytes were already archived under a
// different local id would go unrecognised and sync/check would ask for it on
// every run, forever.
func (s *Store) RecordAsset(ctx context.Context, a Asset) (id string, inserted bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insert = `
		insert into assets (sha256, md5, byte_size, original_filename, ext,
		                    content_type, media_kind, captured_at, device_id, local_id,
		                    live_parent_local_id, live_state)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		        case when $11 = '' then 'none' else 'pending' end)
		on conflict (sha256) do nothing
		returning id`

	mediaKind := a.MediaKind
	if mediaKind == "" {
		mediaKind = MediaImage
	}

	// Only a video can be a Live Photo's other half. Dropping the declaration
	// rather than trusting it keeps a client that mislabels a still from hiding
	// a photo out of the timeline, which is the one failure here that loses
	// something the archive is supposed to show.
	liveParent := a.LiveParentLocalID
	if mediaKind != MediaVideo {
		liveParent = ""
	}

	err = tx.QueryRow(ctx, insert,
		a.SHA256, a.MD5, a.ByteSize, a.OriginalFilename, a.Ext,
		a.ContentType, mediaKind, a.CapturedAt, a.DeviceID, a.LocalID, liveParent,
	).Scan(&id)
	switch {
	case err == nil:
		inserted = true
	case errors.Is(err, pgx.ErrNoRows):
		// ON CONFLICT DO NOTHING returns no rows, so the existing id needs a read.
		if err := tx.QueryRow(ctx, `select id from assets where sha256 = $1`, a.SHA256).Scan(&id); err != nil {
			return "", false, fmt.Errorf("look up existing asset: %w", err)
		}
	default:
		return "", false, fmt.Errorf("insert asset: %w", err)
	}

	if a.DeviceID != "" && a.LocalID != "" {
		const upsert = `
			insert into device_assets (device_id, local_id, asset_id, modified_at)
			values ($1, $2, $3, $4)
			on conflict (device_id, local_id)
			do update set asset_id = excluded.asset_id, modified_at = excluded.modified_at`
		if _, err := tx.Exec(ctx, upsert, a.DeviceID, a.LocalID, id, a.ModifiedAt); err != nil {
			return "", false, fmt.Errorf("record device mapping: %w", err)
		}
	}

	// After the mapping, because half of this reads it back.
	if err := linkLivePair(ctx, tx, a, id, liveParent); err != nil {
		return "", false, err
	}

	// The metadata job is committed with the asset row, not after it. Enqueuing
	// separately would leave a window where a crash lands an asset that no
	// worker will ever pick up, and nothing would notice: the phone has its ack
	// and will never send those bytes again.
	//
	// Only on insert. A duplicate already has its derivatives, or a job queued
	// to build them.
	if inserted {
		if err := jobs.Enqueue(ctx, tx, jobs.KindMetadata, id); err != nil {
			return "", false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit transaction: %w", err)
	}
	return id, inserted, nil
}

// linkLivePair joins a Live Photo's two halves, from whichever of them just
// landed.
//
// Both directions are needed because either can arrive first. The phone queues
// the still and its paired video with the same capture time, and the queue
// orders by capture time, so which of the two goes up first is not decided
// anywhere — and a run interrupted between them can deliver them minutes apart.
//
// Nothing here fails a commit. A pairing that does not resolve costs a hover
// animation; refusing the upload over it would cost the photo.
func linkLivePair(ctx context.Context, tx pgx.Tx, a Asset, id, liveParent string) error {
	if liveParent != "" {
		// The video: find the still, if the archive has it.
		const link = `
			update assets set live_parent_asset_id = d.asset_id
			from device_assets d
			where assets.id = $1::uuid
			  and assets.live_parent_asset_id is null
			  and d.device_id = $2 and d.local_id = $3`
		if _, err := tx.Exec(ctx, link, id, a.DeviceID, liveParent); err != nil {
			return fmt.Errorf("link paired video to its still: %w", err)
		}
		return nil
	}

	if a.DeviceID == "" || a.LocalID == "" {
		return nil
	}

	// The still: adopt any video that was already waiting for it.
	const adopt = `
		update assets set live_parent_asset_id = $1::uuid
		where device_id = $2
		  and live_parent_local_id = $3
		  and live_parent_asset_id is null`
	if _, err := tx.Exec(ctx, adopt, id, a.DeviceID, a.LocalID); err != nil {
		return fmt.Errorf("link still to its paired video: %w", err)
	}
	return nil
}

// LiveVideoFor returns the paired video belonging to a still, which is what the
// gallery's hover animation and the viewer's press-and-hold play.
func (s *Store) LiveVideoFor(ctx context.Context, stillID string) (Asset, error) {
	// Ordered and limited because two devices delivering the same still resolve
	// to one asset and can each attach their own copy of the video to it. Both
	// show the same three seconds; the first one archived is as good an answer
	// as the question has.
	row := s.pool.QueryRow(ctx, `select `+assetColumns+`
		from assets where live_parent_asset_id = $1::uuid
		order by uploaded_at, id limit 1`, stillID)
	a, err := scanAsset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, fmt.Errorf("load paired video: %w", err)
	}
	return a, nil
}

// KnownMappings returns, for the refs asked about, the local ids this device has
// already delivered, mapped to their asset id. A ref is only considered known
// when its modification time still matches what was recorded, so a photo edited
// in place is reported as unknown and gets re-checked by content.
//
// `is not distinct from` makes two nulls match, which is what lets rows written
// before modification times were tracked resolve at all.
func (s *Store) KnownMappings(ctx context.Context, deviceID string, refs []LocalRef) (map[string]string, error) {
	known := make(map[string]string, len(refs))
	if deviceID == "" || len(refs) == 0 {
		return known, nil
	}

	localIDs := make([]string, len(refs))
	modified := make([]*time.Time, len(refs))
	for i, ref := range refs {
		localIDs[i] = ref.LocalID
		modified[i] = ref.ModifiedAt
	}

	const query = `
		select d.local_id, d.asset_id::text
		from device_assets d
		join unnest($2::text[], $3::timestamptz[]) as r(local_id, modified_at)
		  on r.local_id = d.local_id
		where d.device_id = $1
		  and d.modified_at is not distinct from r.modified_at`

	rows, err := s.pool.Query(ctx, query, deviceID, localIDs, modified)
	if err != nil {
		return nil, fmt.Errorf("look up device mappings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var localID, assetID string
		if err := rows.Scan(&localID, &assetID); err != nil {
			return nil, fmt.Errorf("scan device mapping: %w", err)
		}
		known[localID] = assetID
	}
	return known, rows.Err()
}

// AssetsByContent resolves content keys to archived assets. Keys that matched
// nothing are absent from the result; keys that matched more than one asset are
// present with Matches > 1 so the caller can refuse to pick one.
func (s *Store) AssetsByContent(ctx context.Context, keys []ContentKey) (map[ContentKey]ContentMatch, error) {
	found := make(map[ContentKey]ContentMatch, len(keys))
	if len(keys) == 0 {
		return found, nil
	}

	md5s := make([]string, len(keys))
	sizes := make([]int64, len(keys))
	for i, k := range keys {
		md5s[i] = k.MD5
		sizes[i] = k.ByteSize
	}

	const query = `
		select k.md5, k.byte_size, count(*)::int, min(a.id::text)
		from unnest($1::text[], $2::bigint[]) as k(md5, byte_size)
		join assets a on a.md5 = k.md5 and a.byte_size = k.byte_size
		group by k.md5, k.byte_size`

	rows, err := s.pool.Query(ctx, query, md5s, sizes)
	if err != nil {
		return nil, fmt.Errorf("look up assets by content: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key ContentKey
		var match ContentMatch
		if err := rows.Scan(&key.MD5, &key.ByteSize, &match.Matches, &match.AssetID); err != nil {
			return nil, fmt.Errorf("scan content match: %w", err)
		}
		found[key] = match
	}
	return found, rows.Err()
}

// RecordMappings associates local ids with already-archived assets, which is
// what lets a content match answer "have" on the next run without re-uploading.
//
// The caller must not pass the same local id twice: Postgres refuses to let one
// statement's ON CONFLICT DO UPDATE touch a row more than once.
func (s *Store) RecordMappings(ctx context.Context, deviceID string, mappings []Mapping) error {
	if deviceID == "" || len(mappings) == 0 {
		return nil
	}

	localIDs := make([]string, len(mappings))
	assetIDs := make([]string, len(mappings))
	modified := make([]*time.Time, len(mappings))
	for i, m := range mappings {
		localIDs[i] = m.LocalID
		assetIDs[i] = m.AssetID
		modified[i] = m.ModifiedAt
	}

	const upsert = `
		insert into device_assets (device_id, local_id, asset_id, modified_at)
		select $1, m.local_id, m.asset_id::uuid, m.modified_at
		from unnest($2::text[], $3::text[], $4::timestamptz[]) as m(local_id, asset_id, modified_at)
		on conflict (device_id, local_id)
		do update set asset_id = excluded.asset_id, modified_at = excluded.modified_at`

	if _, err := s.pool.Exec(ctx, upsert, deviceID, localIDs, assetIDs, modified); err != nil {
		return fmt.Errorf("record device mappings: %w", err)
	}
	return nil
}

func (s *Store) Asset(ctx context.Context, id string) (Asset, error) {
	row := s.pool.QueryRow(ctx, `select `+assetColumns+` from assets where id = $1`, id)
	a, err := scanAsset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, fmt.Errorf("load asset: %w", err)
	}
	return a, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAsset(s scanner) (Asset, error) {
	var a Asset
	err := s.Scan(&a.ID, &a.SHA256, &a.MD5, &a.ByteSize, &a.OriginalFilename, &a.Ext,
		&a.ContentType, &a.MediaKind, &a.CapturedAt, &a.UploadedAt, &a.DeviceID, &a.LocalID,
		&a.Width, &a.Height, &a.Orientation, &a.DurationSeconds,
		&a.CameraMake, &a.CameraModel, &a.Lens,
		&a.GPSLat, &a.GPSLon, &a.ExifCapturedAt, &a.ExifOffsetMinutes,
		&a.LiveParentLocalID, &a.LiveParentAssetID, &a.LiveState,
		&a.SortTime, &a.DerivedState, &a.PlaybackState)
	return a, err
}
