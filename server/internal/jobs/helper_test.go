package jobs_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

const (
	adminURL = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	// Its own database, like every other package: `go test ./...` runs packages
	// concurrently and a shared one means they truncate each other's rows.
	testDBName = "photobackup_test_jobs"
)

// These tests are an external test package (jobs_test) so they can import db to
// run the migrations. db imports jobs, and an in-package test importing db
// would be a cycle.

func testQueue(t *testing.T) (*jobs.Queue, *db.Store) {
	t.Helper()
	ctx := context.Background()

	admin := adminURL
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ensureTestDatabase(t, ctx, admin)

	store, err := db.Open(ctx, withDatabase(t, admin, testDBName))
	if err != nil {
		t.Fatalf("open test database: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	t.Cleanup(store.Close)

	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, "truncate table assets, device_assets, jobs cascade"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return jobs.NewQueue(store.Pool()), store
}

// newAsset inserts an asset to hang jobs off, since jobs.asset_id is a foreign
// key. RecordAsset already enqueues a metadata job, so the caller usually wants
// to clear the queue or account for it.
func newAsset(t *testing.T, store *db.Store) string {
	t.Helper()
	sum := make([]byte, 32)
	if _, err := rand.Read(sum); err != nil {
		t.Fatalf("random digest: %v", err)
	}
	sha := hex.EncodeToString(sum)

	id, _, err := store.RecordAsset(context.Background(), db.Asset{
		SHA256:           sha,
		MD5:              sha[:32],
		ByteSize:         1024,
		OriginalFilename: "IMG_0001.HEIC",
		Ext:              ".heic",
		ContentType:      "image/heic",
		MediaKind:        db.MediaImage,
		DeviceID:         "test-device",
		LocalID:          sha[:16],
	})
	if err != nil {
		t.Fatalf("record asset: %v", err)
	}
	return id
}

// clearJobs removes the metadata jobs RecordAsset enqueued, so a test can start
// from a queue holding exactly what it put there.
func clearJobs(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), "delete from jobs"); err != nil {
		t.Fatalf("clear jobs: %v", err)
	}
}

func withDatabase(t *testing.T, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func ensureTestDatabase(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx, "select exists (select 1 from pg_database where datname = $1)", testDBName).Scan(&exists); err != nil {
		t.Fatalf("look up test database: %v", err)
	}
	if exists {
		return
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("create database %s", pgx.Identifier{testDBName}.Sanitize())); err != nil {
		t.Fatalf("create test database: %v", err)
	}
}
