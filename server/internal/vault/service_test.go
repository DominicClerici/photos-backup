package vault

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
)

// The round trip, end to end and with nothing stubbed: real Postgres, real
// files, real X25519 and real AES-GCM. It is the one test that can catch the
// failure this whole feature is about — a photograph that goes into the vault
// and does not come back out the same photograph.

const (
	adminURL   = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	testDBName = "photobackup_test_vault"
)

type harness struct {
	*Service
	store *db.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	admin := adminURL
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ensureDatabase(t, ctx, admin)

	store, err := db.Open(ctx, withDatabase(t, admin, testDBName))
	if err != nil {
		t.Fatalf("connect to test database: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool().Exec(ctx,
		"truncate table assets, device_assets, jobs, albums, vault_people, vault_secret cascade"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	dir := t.TempDir()
	return &harness{
		store: store,
		Service: &Service{
			Store:            store,
			Blobs:            blobstore.New(filepath.Join(dir, "photos")),
			Derivatives:      derivstore.New(filepath.Join(dir, "derivatives")),
			VaultBlobs:       NewStore(filepath.Join(dir, "photos", "vault")),
			VaultDerivatives: NewStore(filepath.Join(dir, "derivatives", "vault")),
			Keeper:           NewKeeper(time.Minute),
		},
	}
}

// archive stores one original and a thumbnail for it, the way an upload and the
// metadata job between them would.
func (h *harness) archive(t *testing.T, i int, body []byte) db.Asset {
	t.Helper()
	ctx := context.Background()

	result, err := h.Blobs.Put(bytes.NewReader(body), ".heic", blobstore.Expected{
		MD5: md5Of(body), Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}

	captured := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(-i) * time.Hour)
	id, _, err := h.store.RecordAsset(ctx, db.Asset{
		SHA256: result.SHA256, MD5: result.MD5, ByteSize: result.Size,
		OriginalFilename: fmt.Sprintf("IMG_%04d.HEIC", i), Ext: ".heic",
		ContentType: "image/heic", MediaKind: db.MediaImage,
		CapturedAt: &captured, DeviceID: "test-device", LocalID: fmt.Sprintf("local-%d", i),
	})
	if err != nil {
		t.Fatalf("record asset: %v", err)
	}

	if err := h.Derivatives.Write(result.SHA256, derivstore.Thumb, func(w io.Writer) error {
		_, err := w.Write([]byte("thumbnail bytes for " + result.SHA256))
		return err
	}); err != nil {
		t.Fatalf("write thumbnail: %v", err)
	}

	asset, err := h.store.Asset(ctx, id)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	return asset
}

func TestAPhotoSurvivesTheRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	original := make([]byte, 3*chunkSize+911)
	if _, err := rand.Read(original); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	asset := h.archive(t, 1, original)

	if err := h.Setup(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	candidates, err := h.store.VaultCandidates(ctx, db.Selection{IDs: []string{asset.ID}})
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	if _, err := h.Add(ctx, db.VaultHidden, candidates); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The plaintext is gone from both trees, which is the point of the whole
	// exercise and the one assertion that would still matter if every other
	// line here were deleted.
	if _, err := os.Stat(h.Blobs.Path(asset.SHA256, asset.Ext)); !os.IsNotExist(err) {
		t.Error("the original is still readable in the blob tree")
	}
	if h.Derivatives.Exists(asset.SHA256, derivstore.Thumb) {
		t.Error("the thumbnail is still readable on the derivatives disk")
	}
	if !h.VaultBlobs.Exists(asset.SHA256, "") {
		t.Fatal("nothing was written to the vault")
	}
	if !h.VaultDerivatives.Exists(asset.SHA256, derivstore.Thumb) {
		t.Error("the thumbnail was not sealed; the grid would have shown the photo anyway")
	}

	// A locked vault reads nothing, including from a process that has just
	// written it.
	h.Keeper.Lock()
	if _, err := h.OpenOriginal(asset.SHA256); err == nil {
		t.Fatal("a locked vault handed over the original")
	}
	if _, err := h.Index(ctx, db.VaultHidden); err == nil {
		t.Fatal("a locked vault built an index")
	}

	if err := h.Unlock(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	index, err := h.Index(ctx, db.VaultHidden)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(index.Items) != 1 {
		t.Fatalf("the index holds %d items, want 1", len(index.Items))
	}
	if got := index.Items[0].Filename(); got != "IMG_0001.HEIC" {
		t.Errorf("filename = %q, want the one the scrub took off the row", got)
	}

	if _, err := h.Remove(ctx, []string{asset.ID}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// blobstore.Put verifies the digest and the length on the way back in, so
	// reaching here at all means the bytes are the bytes. Reading them is the
	// belt to that braces.
	back, err := os.ReadFile(h.Blobs.Path(asset.SHA256, asset.Ext))
	if err != nil {
		t.Fatalf("read the restored original: %v", err)
	}
	if len(back) != len(original) {
		t.Fatalf("the restored original is %d bytes, want %d", len(back), len(original))
	}
	for i := range back {
		if back[i] != original[i] {
			t.Fatalf("the restored original differs at byte %d", i)
		}
	}
	if !h.Derivatives.Exists(asset.SHA256, derivstore.Thumb) {
		t.Error("the thumbnail did not come back")
	}
	if h.VaultBlobs.Exists(asset.SHA256, "") {
		t.Error("the sealed copy was left behind after a restore")
	}
}

// The one window in which a hidden photograph is still readable: a crash
// between the transaction and the unlink. The sweep is what closes it.
func TestTheSweepRemovesPlaintextLeftBehind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 2, []byte("a photograph"))
	if err := h.Setup(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	candidates, err := h.store.VaultCandidates(ctx, db.Selection{IDs: []string{asset.ID}})
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	if _, err := h.Add(ctx, db.VaultArchive, candidates); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Put the plaintext back exactly where an interrupted hide would have left
	// it — under the extension the scrub has already erased from the row, which
	// is why the sweep has to look by digest rather than by name.
	strayed := h.Blobs.Path(asset.SHA256, asset.Ext)
	if err := os.MkdirAll(filepath.Dir(strayed), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(strayed, []byte("a photograph"), 0o644); err != nil {
		t.Fatalf("write stray plaintext: %v", err)
	}

	cleaned, err := h.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if cleaned != 1 {
		t.Errorf("the sweep cleaned %d, want 1", cleaned)
	}
	if _, err := os.Stat(strayed); !os.IsNotExist(err) {
		t.Error("the stray plaintext is still there")
	}
	if !h.VaultBlobs.Exists(asset.SHA256, "") {
		t.Error("the sweep removed the sealed copy as well")
	}
}

func TestChangingThePasswordKeepsTheVaultReadable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 3, []byte("a photograph worth hiding"))
	if err := h.Setup(ctx, "the first password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	candidates, err := h.store.VaultCandidates(ctx, db.Selection{IDs: []string{asset.ID}})
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	if _, err := h.Add(ctx, db.VaultHidden, candidates); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := h.ChangePassword(ctx, "the first password", "the second password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	h.Keeper.Lock()

	if err := h.Unlock(ctx, "the first password"); err == nil {
		t.Error("the old password still opens the vault")
	}
	if err := h.Unlock(ctx, "the second password"); err != nil {
		t.Fatalf("the new password does not open the vault: %v", err)
	}
	index, err := h.Index(ctx, db.VaultHidden)
	if err != nil || len(index.Items) != 1 {
		t.Fatalf("the vault is unreadable after a password change: %v (%d items)", err, len(index.Items))
	}
}

// Hiding must work with no password in hand — it is the property the whole
// keypair design exists for, and the one a simpler "derive a key from the
// password" scheme would silently lose.
func TestHidingWorksWhileTheVaultIsLocked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 4, []byte("a photograph"))
	if err := h.Setup(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	h.Keeper.Lock()

	candidates, err := h.store.VaultCandidates(ctx, db.Selection{IDs: []string{asset.ID}})
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	if _, err := h.Add(ctx, db.VaultArchive, candidates); err != nil {
		t.Fatalf("Add on a locked vault: %v", err)
	}
	if h.Keeper.Unlocked() {
		t.Error("hiding something unlocked the vault")
	}
	if !h.VaultBlobs.Exists(asset.SHA256, "") {
		t.Error("nothing was sealed")
	}
	// And taking it back out still does not.
	if _, err := h.Remove(ctx, []string{asset.ID}); err == nil {
		t.Error("a locked vault gave a photograph back")
	}
}

// --- plumbing -------------------------------------------------------------

func md5Of(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func ensureDatabase(t *testing.T, ctx context.Context, admin string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to Postgres: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		"select exists (select 1 from pg_database where datname = $1)", testDBName).Scan(&exists); err != nil {
		t.Fatalf("look up test database: %v", err)
	}
	if exists {
		return
	}
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("create database %s", pgx.Identifier{testDBName}.Sanitize())); err != nil {
		t.Fatalf("create test database: %v", err)
	}
}

func withDatabase(t *testing.T, raw, name string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
