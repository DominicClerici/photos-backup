package api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
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
	srv          *Server
	devices      *devices.Store
	photosRoot   string
	derivRoot    string
	manifestPath string
	// token and deviceID are a real pairing, redeemed through the real code
	// path. The tests authenticate the way the phone does rather than through a
	// door only they can use, so "does the write path require a token" is
	// answered by every test in the package instead of by one of them.
	token    string
	deviceID string
	nudges   atomic.Int64
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
	derivRoot := t.TempDir()
	manifestPath := filepath.Join(root, "manifest.jsonl")

	h := &harness{
		store:        store,
		photosRoot:   root,
		derivRoot:    derivRoot,
		manifestPath: manifestPath,
	}
	h.devices = devices.New(store.Pool())
	h.srv = &Server{
		Store:       store,
		Blobs:       blobstore.New(root),
		Derivatives: derivstore.New(derivRoot),
		Manifest:    manifest.New(manifestPath),
		Converter:   derive.New(),
		Queue:       jobs.NewQueue(store.Pool()),
		Uploads:     uploads.New(filepath.Join(root, "incoming")),
		Devices:     h.devices,
		Nudge:       func() { h.nudges.Add(1) },
		Log:         slog.New(slog.DiscardHandler),
	}

	ts := httptest.NewServer(h.srv.Handler())
	t.Cleanup(ts.Close)
	h.server = ts

	h.deviceID, h.token = h.pair(t, "test phone")
	return h
}

// pair redeems a fresh code, returning the device id and its token.
func (h *harness) pair(t *testing.T, name string) (deviceID, token string) {
	t.Helper()
	ctx := context.Background()

	code, _, err := h.devices.CreateCode(ctx, time.Minute, "test")
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}
	resp := h.postJSON(t, "/v1/pair", fmt.Sprintf(`{"code":%q,"name":%q,"platform":"test"}`, code, name))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("pair returned %d: %s", resp.StatusCode, body)
	}
	var out struct{ DeviceID, Token string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode pair response: %v", err)
	}
	if out.Token == "" || out.DeviceID == "" {
		t.Fatal("pairing returned no token or no device id")
	}
	return out.DeviceID, out.Token
}

// derive runs the real derivative pipeline for one asset, so the endpoints that
// serve stored files have something to serve. The api package deliberately does
// not depend on the worker in production code; this is the test wiring it up to
// exercise the pieces together.
func (h *harness) derive(t *testing.T, assetID string) {
	t.Helper()
	ctx := context.Background()

	asset, err := h.store.Asset(ctx, assetID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}

	src := h.srv.Blobs.Path(asset.SHA256, asset.Ext)
	data, err := exifdata.New().Read(ctx, src)
	if err != nil {
		t.Fatalf("read exif: %v", err)
	}

	if err := h.srv.Derivatives.Write(asset.SHA256, derivstore.Thumb, func(w io.Writer) error {
		return h.srv.Converter.Thumb(ctx, src, w)
	}); err != nil {
		t.Fatalf("write thumb: %v", err)
	}

	width, height := data.DisplaySize()
	err = h.store.ApplyMetadata(ctx, assetID, db.Metadata{
		Width: width, Height: height, Orientation: data.Orientation,
		CameraMake: data.CameraMake, CameraModel: data.CameraModel, Lens: data.Lens,
		GPSLat: data.GPSLat, GPSLon: data.GPSLon,
		ExifCapturedAt: data.CapturedAt, ExifOffsetMinutes: data.OffsetMinutes,
	})
	if err != nil {
		t.Fatalf("apply metadata: %v", err)
	}
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
		"X-Photo-Device-Id":   h.deviceID,
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
	h.authorize(req)
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

// Fixtures live at the module root, shared with the media packages.
func loadFixture(t *testing.T) []byte { return loadNamedFixture(t, "sample.heic") }

func loadNamedFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// getWith issues a GET carrying extra headers, for the conditional-request
// tests.
func (h *harness) getWith(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) postJSON(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	h.authorize(req)
	return h.do(t, req)
}

// authorize adds the harness's device token, unless the caller already set an
// Authorization header — which is how the rejection tests send a bad one.
func (h *harness) authorize(req *http.Request) {
	if h.token != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
}

func (h *harness) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
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
	// Named rather than `cascade`, so a future table with a foreign key here has
	// to be added deliberately instead of silently wiped.
	_, _ = conn.Exec(ctx, "truncate table assets, device_assets, jobs, pairing_codes, devices")
}
