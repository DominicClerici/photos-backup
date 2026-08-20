package worker

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/video"
)

const (
	adminURL   = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	testDBName = "photobackup_test_worker"
)

// These tests run the real pipeline: real Postgres, real exiftool, real
// ImageMagick, real ffmpeg, real fixture files. Nothing is stubbed, because the
// only thing this package does is sequence those tools — a test with a stubbed
// ffmpeg would be verifying the stub.
type harness struct {
	*Runner
	store *db.Store
	root  string
}

func newHarness(t *testing.T) *harness {
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
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, "truncate table assets, device_assets, jobs, merge_groups cascade"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	root := t.TempDir()
	runner := New(Deps{
		Store:       store,
		Queue:       jobs.NewQueue(store.Pool()),
		Blobs:       blobstore.New(filepath.Join(root, "photos")),
		Derivatives: derivstore.New(filepath.Join(root, "derivatives")),
		Images:      derive.New(),
		Video:       video.New(),
		Exif:        exifdata.New(),
		// The merge job archives an original, and refuses to without somewhere
		// to record it. Every harness gets one so that "does the log get
		// written" is answered by the test that cares rather than by its
		// absence everywhere.
		Manifest: manifest.New(filepath.Join(root, "photos", "manifest.jsonl")),
		Log:      slog.New(slog.DiscardHandler),
	})
	// The fixtures are tiny; slow encoder settings would only make the suite drag.
	runner.Video.CRF = 30

	return &harness{Runner: runner, store: store, root: root}
}

// ingest puts a fixture through the same path an upload takes: bytes into the
// blob store, then a row and its metadata job into the database.
func (h *harness) ingest(t *testing.T, fixture, mediaKind string) db.Asset {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	return h.ingestBytes(t, fixture, mediaKind, body)
}

// ingestBytes is the same path for content that is generated rather than
// checked in — a Snapchat caption layer, whose whole job is to be a shape and a
// size no fixture happens to be.
func (h *harness) ingestBytes(t *testing.T, name, mediaKind string, body []byte) db.Asset {
	t.Helper()

	sum := md5.Sum(body)
	ext := filepath.Ext(name)
	fixture := name

	res, err := h.Blobs.Put(bytes.NewReader(body), ext, blobstore.Expected{
		MD5:  hex.EncodeToString(sum[:]),
		Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}

	id, _, err := h.store.RecordAsset(context.Background(), db.Asset{
		SHA256:           res.SHA256,
		MD5:              res.MD5,
		ByteSize:         res.Size,
		OriginalFilename: fixture,
		Ext:              ext,
		ContentType:      "application/octet-stream",
		MediaKind:        mediaKind,
		DeviceID:         "test-device",
		LocalID:          fixture,
	})
	if err != nil {
		t.Fatalf("record asset: %v", err)
	}

	asset, err := h.store.Asset(context.Background(), id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	return asset
}

// claimAndRun takes the next job of a kind and executes it exactly as a pool
// worker would, so the retry and state bookkeeping under test is the real one.
func (h *harness) claimAndRun(t *testing.T, kind jobs.Kind) jobs.Job {
	t.Helper()
	job, err := h.Queue.Claim(context.Background(), []jobs.Kind{kind}, "test-worker")
	if err != nil {
		t.Fatalf("claim %s job: %v", kind, err)
	}
	h.execute(context.Background(), "test-worker", job)
	return job
}

func (h *harness) reload(t *testing.T, id string) db.Asset {
	t.Helper()
	asset, err := h.store.Asset(context.Background(), id)
	if err != nil {
		t.Fatalf("reload asset: %v", err)
	}
	return asset
}

// waitFor polls until cond holds or the deadline passes. The pools are
// asynchronous, so there is nothing to synchronise on but the outcome.
func (h *harness) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) jobState(t *testing.T, assetID string, kind jobs.Kind) string {
	t.Helper()
	var state string
	err := h.store.Pool().QueryRow(context.Background(),
		`select state from jobs where asset_id = $1::uuid and kind = $2`, assetID, string(kind)).Scan(&state)
	if err != nil {
		t.Fatalf("read job state: %v", err)
	}
	return state
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

// jobStateAndAttempts reads a job row by id, for the tests that care what a
// failure cost rather than only that one happened.
func (h *harness) jobStateAndAttempts(t *testing.T, id int64) (state string, attempts int) {
	t.Helper()
	if err := h.store.Pool().QueryRow(context.Background(),
		"select state, attempts from jobs where id = $1", id).Scan(&state, &attempts); err != nil {
		t.Fatalf("read job %d: %v", id, err)
	}
	return state, attempts
}

// jobStateOrNone is jobState for the tests where the absence of a job is the
// thing being asserted — a vision pass on a machine with no photo-ml, or one
// queued before the renditions it reads exist.
func (h *harness) jobStateOrNone(t *testing.T, assetID string, kind jobs.Kind) string {
	t.Helper()
	var state string
	err := h.store.Pool().QueryRow(context.Background(),
		`select state from jobs where asset_id = $1::uuid and kind = $2`, assetID, string(kind)).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read job state: %v", err)
	}
	return state
}
