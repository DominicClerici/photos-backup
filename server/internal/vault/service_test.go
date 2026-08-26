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

// The words make the same round trip the picture does, through real X25519 and
// real AES-GCM.
func TestTheWordsSurviveTheRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 4, []byte("a photograph of a receipt"))
	if err := h.store.PutDescription(ctx, asset.ID, db.CaptionModel,
		"a man asleep on a sofa in a bedroom",
		[]db.Tag{{Name: "sofa", Confidence: 0.9}}); err != nil {
		t.Fatalf("PutDescription: %v", err)
	}
	if err := h.store.PutOCR(ctx, asset.ID, db.OCRModel,
		"ACCOUNT 1234 5678 BALANCE 4210.55"); err != nil {
		t.Fatalf("PutOCR: %v", err)
	}
	if err := h.Setup(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	h.hide(t, db.VaultHidden, asset.ID)
	if captions := h.countCaptions(t, asset.ID); captions != 0 {
		t.Fatalf("a hidden photograph still has %d captions in the clear", captions)
	}
	// The sealed document is opened with the private key and nothing else, so a
	// locked vault is as opaque about the words as it is about the picture.
	h.Keeper.Lock()
	if _, err := h.Remove(ctx, []string{asset.ID}); err == nil {
		t.Fatal("a locked vault handed a photograph back")
	}
	if err := h.Unlock(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	if _, err := h.Remove(ctx, []string{asset.ID}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	analysis, err := h.store.AssetAnalysis(ctx, asset.ID)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if analysis.Caption != "a man asleep on a sofa in a bedroom" {
		t.Errorf("the caption came back as %q", analysis.Caption)
	}
	if analysis.Text != "ACCOUNT 1234 5678 BALANCE 4210.55" {
		t.Errorf("the recognised text came back as %q", analysis.Text)
	}
	if len(analysis.Tags) != 1 || analysis.Tags[0].Name != "sofa" {
		t.Errorf("the tags came back as %+v", analysis.Tags)
	}
}

// The sweep, which is how an archive hidden before migration 0023 catches up:
// it seals what it finds rather than deleting it, needs no password, and a
// restore afterwards is still whole.
func TestTheSweepSealsWordsLeftInTheClear(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 5, []byte("a photograph of a bank statement"))
	if err := h.Setup(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	h.hide(t, db.VaultHidden, asset.ID)

	// What every archive hidden under an older build looks like: a row in the
	// vault, nothing in sealed_analysis, and the caption still in the table.
	if _, err := h.store.Pool().Exec(ctx, `
		insert into asset_descriptions (asset_id, model, caption)
		values ($1::uuid, $2, 'a man asleep on a sofa in a bedroom')`,
		asset.ID, db.CaptionModel); err != nil {
		t.Fatalf("leak a caption: %v", err)
	}
	if _, err := h.store.Pool().Exec(ctx, `
		insert into asset_ocr (asset_id, model, text)
		values ($1::uuid, $2, 'ACCOUNT 1234 5678 BALANCE 4210.55')`,
		asset.ID, db.OCRModel); err != nil {
		t.Fatalf("leak the recognised text: %v", err)
	}

	// Locked, deliberately. Putting something into the vault has never needed
	// the password and this is that operation arriving late — a sweep that
	// waited for somebody to unlock would never run on the archives that need
	// it most.
	h.Keeper.Lock()
	sealed, err := h.ReconcileAnalysis(ctx)
	if err != nil {
		t.Fatalf("ReconcileAnalysis: %v", err)
	}
	if sealed != 1 {
		t.Fatalf("the sweep sealed %d assets, want 1", sealed)
	}
	if captions := h.countCaptions(t, asset.ID); captions != 0 {
		t.Errorf("the sweep left %d captions behind", captions)
	}
	// Idempotent: the second tick has nothing to find, which is what makes this
	// affordable on an hourly timer.
	if again, err := h.ReconcileAnalysis(ctx); err != nil || again != 0 {
		t.Errorf("a second sweep sealed %d assets (err %v), want none", again, err)
	}

	// Sealed rather than deleted, which is the whole difference between this
	// and the migration that could not have been written.
	//
	// A stray tsvector on its own is the other half of the same sweep, and the
	// one thing here with nothing to seal: the recipe is in migration 0018 and
	// the row is rebuilt from the columns on the way back out.
	if _, err := h.store.Pool().Exec(ctx, `
		insert into asset_search (asset_id, tsv)
		values ($1::uuid, to_tsvector('english', 'IMG 0005 HEIC'))`, asset.ID); err != nil {
		t.Fatalf("leak a tsvector: %v", err)
	}
	if swept, err := h.ReconcileAnalysis(ctx); err != nil || swept != 1 {
		t.Fatalf("the sweep took %d stray search rows (err %v), want 1", swept, err)
	}
	var indexed int
	if err := h.store.Pool().QueryRow(ctx,
		`select count(*) from asset_search where asset_id = $1::uuid`, asset.ID).
		Scan(&indexed); err != nil {
		t.Fatalf("count search rows: %v", err)
	}
	if indexed != 0 {
		t.Errorf("a hidden photograph is still in the search index")
	}

	if err := h.Unlock(ctx, "a password for the vault"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := h.Remove(ctx, []string{asset.ID}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	analysis, err := h.store.AssetAnalysis(ctx, asset.ID)
	if err != nil {
		t.Fatalf("AssetAnalysis: %v", err)
	}
	if analysis.Caption != "a man asleep on a sofa in a bedroom" {
		t.Errorf("the swept caption came back as %q", analysis.Caption)
	}
	if analysis.Text != "ACCOUNT 1234 5678 BALANCE 4210.55" {
		t.Errorf("the swept text came back as %q", analysis.Text)
	}
}

// hide runs one asset through the whole write path: resolve, seal, commit, drop
// the plaintext.
func (h *harness) hide(t *testing.T, bucket, assetID string) {
	t.Helper()
	ctx := context.Background()

	candidates, err := h.store.VaultCandidates(ctx, db.Selection{IDs: []string{assetID}})
	if err != nil {
		t.Fatalf("VaultCandidates: %v", err)
	}
	if _, err := h.Add(ctx, bucket, candidates); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func (h *harness) countCaptions(t *testing.T, assetID string) int {
	t.Helper()

	var n int
	if err := h.store.Pool().QueryRow(context.Background(),
		`select count(*) from asset_descriptions where asset_id = $1::uuid`, assetID).
		Scan(&n); err != nil {
		t.Fatalf("count captions: %v", err)
	}
	return n
}
