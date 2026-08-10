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
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a lookup by id matches no row.
var ErrNotFound = errors.New("db: asset not found")

// Asset is one archived original. ID and UploadedAt are assigned by the
// database; every other field comes from the upload.
type Asset struct {
	ID               string
	SHA256           string
	MD5              string
	ByteSize         int64
	OriginalFilename string
	Ext              string
	ContentType      string
	CapturedAt       *time.Time
	UploadedAt       time.Time
	DeviceID         string
	LocalID          string
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
	content_type, captured_at, uploaded_at, device_id, local_id`

// InsertAsset records an asset, returning the existing row's id when this
// content is already archived. inserted reports whether a new row was created.
func (s *Store) InsertAsset(ctx context.Context, a Asset) (id string, inserted bool, err error) {
	const insert = `
		insert into assets (sha256, md5, byte_size, original_filename, ext,
		                    content_type, captured_at, device_id, local_id)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (sha256) do nothing
		returning id`

	err = s.pool.QueryRow(ctx, insert,
		a.SHA256, a.MD5, a.ByteSize, a.OriginalFilename, a.Ext,
		a.ContentType, a.CapturedAt, a.DeviceID, a.LocalID,
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("insert asset: %w", err)
	}

	// ON CONFLICT DO NOTHING returns no rows, so the existing id needs a read.
	if err := s.pool.QueryRow(ctx, `select id from assets where sha256 = $1`, a.SHA256).Scan(&id); err != nil {
		return "", false, fmt.Errorf("look up existing asset: %w", err)
	}
	return id, false, nil
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

// RecentAssets returns the newest assets first, by capture time where known.
func (s *Store) RecentAssets(ctx context.Context, limit int) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `select `+assetColumns+`
		from assets
		order by captured_at desc nulls last, uploaded_at desc
		limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()

	var assets []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAsset(s scanner) (Asset, error) {
	var a Asset
	err := s.Scan(&a.ID, &a.SHA256, &a.MD5, &a.ByteSize, &a.OriginalFilename, &a.Ext,
		&a.ContentType, &a.CapturedAt, &a.UploadedAt, &a.DeviceID, &a.LocalID)
	return a, err
}
