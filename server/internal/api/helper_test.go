package api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

const (
	adminURL = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	// Distinct from the db package's database: `go test ./...` runs packages
	// concurrently and they would otherwise truncate each other's rows.
	testDBName = "photobackup_test_api"
)

type harness struct {
	server       *httptest.Server
	store        *db.Store
	photosRoot   string
	manifestPath string
}

// newHarness wires the real server against a real Postgres and a temp photos
// root, so the tests exercise the actual commit ordering rather than a stand-in.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	admin := adminURL
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ensureTestDatabase(t, ctx, admin)
	url := withDatabase(t, admin, testDBName)
	truncateAssets(t, ctx, url)

	store, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open database: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	truncateAssets(t, ctx, url)

	root := t.TempDir()
	manifestPath := filepath.Join(root, "manifest.jsonl")
	srv := &Server{
		Store:     store,
		Blobs:     blobstore.New(root),
		Manifest:  manifest.New(manifestPath),
		Converter: derive.New(),
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &harness{server: ts, store: store, photosRoot: root, manifestPath: manifestPath}
}

// upload posts a body as an asset, letting the caller override any header to
// exercise the rejection paths.
func (h *harness) upload(t *testing.T, body []byte, overrides map[string]string) *http.Response {
	t.Helper()
	sum := md5.Sum(body)

	headers := map[string]string{
		"Content-Type":        "application/octet-stream",
		"X-Photo-Filename":    "IMG_8071.HEIC",
		"X-Photo-Md5":         hex.EncodeToString(sum[:]),
		"X-Photo-Size":        fmt.Sprint(len(body)),
		"X-Photo-Captured-At": "2026-08-01T15:04:05Z",
		"X-Photo-Device-Id":   "iphone-14-pro",
		"X-Photo-Local-Id":    "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001",
	}
	for k, v := range overrides {
		if v == "" {
			delete(headers, k)
			continue
		}
		headers[k] = v
	}

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/assets", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := h.server.Client().Get(h.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// blobFiles lists committed blobs, ignoring the staging directory.
func (h *harness) blobFiles(t *testing.T) []string {
	t.Helper()
	var found []string
	root := filepath.Join(h.photosRoot, "blobs")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() && d.Name() == "tmp" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			found = append(found, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk blobs: %v", err)
	}
	return found
}

func (h *harness) manifestEntries(t *testing.T) []manifest.Entry {
	t.Helper()
	entries, err := manifest.Read(h.manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read manifest: %v", err)
	}
	return entries
}

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sample.heic"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// withDatabase points a connection string at a different database, keeping the
// host and credentials, so TEST_DATABASE_URL selects a server rather than a
// single shared database.
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

func truncateAssets(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return // schema not created yet; the migration runs next
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, "truncate table assets")
}
