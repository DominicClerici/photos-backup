package verify_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

func TestResetErasesTheArchiveAndKeepsTheDevicePaired(t *testing.T) {
	ctx := context.Background()
	a := newArchive(t)

	asset := a.add(t, "IMG_0001.jpg", []byte("first original"), time.Now().Add(-time.Hour))
	a.add(t, "IMG_0002.jpg", []byte("second original"), time.Now())
	writeDerivative(t, a, asset.SHA256, derivstore.Thumb)
	partial := stageUpload(t, a)

	device, token := pairDevice(t, a, "iPhone")

	result, err := verify.Reset(ctx, a.deps, verify.ResetOptions{})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}

	if result.Assets != 2 {
		t.Errorf("reported %d assets erased, want 2", result.Assets)
	}
	if result.Devices != 1 {
		t.Errorf("reported %d devices kept, want 1", result.Devices)
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 0 || counts.Bytes != 0 {
		t.Errorf("database still holds %d assets, %d bytes", counts.Assets, counts.Bytes)
	}

	for _, path := range []string{
		filepath.Join(a.root, "blobs"),
		a.manifestAt,
		filepath.Join(a.root, "incoming"),
		partial,
		a.derivs.Path(asset.SHA256, derivstore.Thumb),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", path)
		}
	}

	// The derivatives root is emptied, not removed: the deployment creates it
	// with an ownership photod could not reproduce.
	if _, err := os.Stat(a.derivRoot); err != nil {
		t.Errorf("derivatives root should still exist: %v", err)
	}

	// The point of the whole command. The token minted before the reset still
	// authenticates after it, so the phone uploads again without re-pairing.
	paired := devices.New(a.store.Pool())
	got, err := paired.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("device stopped working after reset: %v", err)
	}
	if got.ID != device.ID {
		t.Errorf("authenticated as %s, want %s", got.ID, device.ID)
	}
}

func TestResetDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	a := newArchive(t)

	asset := a.add(t, "IMG_0001.jpg", []byte("an original"), time.Now())

	result, err := verify.Reset(ctx, a.deps, verify.ResetOptions{DryRun: true})
	if err != nil {
		t.Fatalf("reset --dry-run: %v", err)
	}
	if result.Assets != 1 {
		t.Errorf("reported %d assets, want 1", result.Assets)
	}
	if len(result.Targets) != 4 {
		t.Fatalf("reported %d targets, want 4", len(result.Targets))
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 1 {
		t.Errorf("dry run emptied the database: %d assets left", counts.Assets)
	}
	if _, err := os.Stat(a.blobPath(asset)); err != nil {
		t.Errorf("dry run removed the blob: %v", err)
	}
	if _, err := os.Stat(a.manifestAt); err != nil {
		t.Errorf("dry run removed the manifest: %v", err)
	}
}

// A second reset has nothing left to do and has to survive saying so, because
// the recovery path for an interrupted reset is running it again.
func TestResetIsIdempotent(t *testing.T) {
	ctx := context.Background()
	a := newArchive(t)
	a.add(t, "IMG_0001.jpg", []byte("an original"), time.Now())

	if _, err := verify.Reset(ctx, a.deps, verify.ResetOptions{}); err != nil {
		t.Fatalf("first reset: %v", err)
	}
	result, err := verify.Reset(ctx, a.deps, verify.ResetOptions{})
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if result.Assets != 0 {
		t.Errorf("second reset erased %d assets, want 0", result.Assets)
	}
	for _, target := range result.Targets {
		if target.Present {
			t.Errorf("%s reported as present after the first reset", target.Path)
		}
	}
}

// Losing ca.key means re-pairing every phone, which is the one outcome reset
// promises to avoid. A TLS directory inside a target has to stop the run before
// anything is touched.
func TestResetRefusesToEraseTheCA(t *testing.T) {
	ctx := context.Background()
	a := newArchive(t)
	a.add(t, "IMG_0001.jpg", []byte("an original"), time.Now())

	_, err := verify.Reset(ctx, a.deps, verify.ResetOptions{
		TLSDir: filepath.Join(a.derivRoot, "tls"),
	})
	if err == nil {
		t.Fatal("reset erased an archive holding the CA")
	}
	if !strings.Contains(err.Error(), "paired again") {
		t.Errorf("error does not explain the consequence: %v", err)
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 1 {
		t.Errorf("refused run still emptied the database: %d assets left", counts.Assets)
	}
}

// A TLS directory beside the archive rather than inside a target is the default
// layout, and it must not trip the check.
func TestResetAllowsATLSDirectoryBesideTheArchive(t *testing.T) {
	ctx := context.Background()
	a := newArchive(t)

	if _, err := verify.Reset(ctx, a.deps, verify.ResetOptions{
		TLSDir: filepath.Join(a.root, "tls"),
	}); err != nil {
		t.Fatalf("reset refused the default layout: %v", err)
	}
}

func pairDevice(t *testing.T, a *archive, name string) (devices.Device, string) {
	t.Helper()
	ctx := context.Background()

	store := devices.New(a.store.Pool())
	code, _, err := store.CreateCode(ctx, 10*time.Minute, "test")
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}
	device, token, err := store.Pair(ctx, code, name, "ios", "127.0.0.1")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return device, token
}

func writeDerivative(t *testing.T, a *archive, sha, suffix string) {
	t.Helper()
	if err := a.derivs.Write(sha, suffix, func(w io.Writer) error {
		_, err := w.Write([]byte("derived bytes"))
		return err
	}); err != nil {
		t.Fatalf("write derivative: %v", err)
	}
}

// stageUpload leaves a partial upload behind, the way a phone that lost Wi-Fi
// mid-transfer would.
func stageUpload(t *testing.T, a *archive) string {
	t.Helper()
	store := uploads.New(filepath.Join(a.root, "incoming"))
	session, err := store.Create(uploads.Declaration{
		DeviceID: "iphone-14-pro", LocalID: "local-partial",
		Filename: "IMG_9999.mov", Size: 1024,
		MD5: "0e2f8b1a6c4d5e7f8091a2b3c4d5e6f7",
	})
	if err != nil {
		t.Fatalf("create upload session: %v", err)
	}
	return store.PartPath(session.ID)
}
