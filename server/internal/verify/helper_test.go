package verify_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

const (
	adminURL = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	// Its own database: `go test ./...` runs packages concurrently, and a
	// shared one means another package truncates assets mid-test.
	testDBName = "photobackup_test_verify"
)

// archive is a real archive on disk backed by a real Postgres, because every
// finding verify reports is about the relationship between the two. A test with
// a stubbed filesystem would be checking the stub.
type archive struct {
	deps       verify.Deps
	store      *db.Store
	blobs      *blobstore.Store
	derivs     *derivstore.Store
	root       string
	derivRoot  string
	manifest   *manifest.Log
	manifestAt string
}

func newArchive(t *testing.T) *archive {
	t.Helper()
	ctx := context.Background()

	admin := adminURL
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ensureTestDatabase(t, ctx, admin)
	dbURL := withDatabase(t, admin, testDBName)

	store, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open database: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	truncate(t, ctx, dbURL)

	root := t.TempDir()
	derivRoot := t.TempDir()
	manifestAt := filepath.Join(root, "manifest.jsonl")

	a := &archive{
		store:      store,
		blobs:      blobstore.New(root),
		derivs:     derivstore.New(derivRoot),
		root:       root,
		derivRoot:  derivRoot,
		manifest:   manifest.New(manifestAt),
		manifestAt: manifestAt,
	}
	a.deps = verify.Deps{
		Store:       store,
		Blobs:       a.blobs,
		Derivatives: a.derivs,
		Uploads:     uploads.New(filepath.Join(root, "incoming")),
		Queue:       jobs.NewQueue(store.Pool()),
		PhotosRoot:  root,
	}
	return a
}

// add archives content the way the upload path does: blob, then manifest line,
// then row.
func (a *archive) add(t *testing.T, filename string, content []byte, captured time.Time) db.Asset {
	t.Helper()
	ctx := context.Background()

	contentType, ext := mediatype.Detect(filename, content)
	res, err := a.blobs.Put(bytes.NewReader(content), ext, blobstore.Expected{
		MD5: md5Hex(content), Size: int64(len(content)),
	})
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}

	entry := manifest.Entry{
		SHA256: res.SHA256, MD5: res.MD5, Size: res.Size,
		Filename: filename, ContentType: contentType, Ext: ext,
		CapturedAt: &captured, DeviceID: "iphone-14-pro",
		LocalID: "local-" + filename, StoredAt: time.Now().UTC(),
	}
	if err := a.manifest.Append(entry); err != nil {
		t.Fatalf("append manifest: %v", err)
	}

	id, _, err := a.store.RecordAsset(ctx, db.Asset{
		SHA256: res.SHA256, MD5: res.MD5, ByteSize: res.Size,
		OriginalFilename: filename, Ext: ext, ContentType: contentType,
		MediaKind: mediatype.Kind(contentType), CapturedAt: &captured,
		DeviceID: "iphone-14-pro", LocalID: "local-" + filename,
	})
	if err != nil {
		t.Fatalf("record asset: %v", err)
	}

	asset, err := a.store.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	return asset
}

// run performs a verify pass.
func (a *archive) run(t *testing.T, opt verify.Options) verify.Report {
	t.Helper()
	report, err := verify.Run(context.Background(), a.deps, opt)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return report
}

// findings returns the findings of one kind.
func findings(r verify.Report, kind verify.Kind) []verify.Finding {
	var out []verify.Finding
	for _, f := range r.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// one returns the single finding of a kind, allowing others alongside it.
func one(t *testing.T, r verify.Report, kind verify.Kind) verify.Finding {
	t.Helper()
	got := findings(r, kind)
	if len(got) != 1 {
		t.Fatalf("got %d %s findings, want 1; all findings: %v", len(got), kind, r.Findings)
	}
	return got[0]
}

// only additionally insists it is the only finding in the report.
func only(t *testing.T, r verify.Report, kind verify.Kind) verify.Finding {
	t.Helper()
	found := one(t, r, kind)
	if len(r.Findings) != 1 {
		t.Errorf("unexpected extra findings: %v", r.Findings)
	}
	return found
}

func (a *archive) blobPath(asset db.Asset) string {
	return a.blobs.Path(asset.SHA256, asset.Ext)
}

// bytesOf and blobExpect put content straight into the blob tree, bypassing the
// manifest — the crash-between-rename-and-append case reindex has to recover.
func bytesOf(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func blobExpect(b []byte) blobstore.Expected {
	return blobstore.Expected{MD5: md5Hex(b), Size: int64(len(b))}
}

// byteCount mirrors the CLI's formatting so failure messages read the same way.
func byteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
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

func truncate(t *testing.T, ctx context.Context, dbURL string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	// devices and pairing_codes go too. Reset's whole claim is about what
	// survives it, and a device left behind by an earlier test would make that
	// assertion pass for the wrong reason.
	_, _ = conn.Exec(ctx, "truncate table assets, device_assets, jobs, devices, pairing_codes cascade")
}
