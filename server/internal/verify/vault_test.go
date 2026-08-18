package verify_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
	verify "github.com/dominicclerici/photos-backup/server/internal/verify"
)

// `verify` runs from a systemd timer at four in the morning and its Critical
// findings are the ones meant to wake somebody up. A photograph that is exactly
// where it was deliberately put must not be one of them.

// seal hides an asset for real: the ciphertext is written and the plaintext
// removed, which is the state verify has to be able to read.
func (a *archive) seal(t *testing.T, asset db.Asset, bucket string) {
	t.Helper()
	ctx := context.Background()

	secret, _, err := vault.Create("a password for the vault")
	if err != nil {
		t.Fatalf("create vault: %v", err)
	}
	to, err := secret.Recipient()
	if err != nil {
		t.Fatalf("recipient: %v", err)
	}
	if _, err := a.vaultBlobs.PutFile(to, asset.SHA256, "", a.blobPath(asset)); err != nil {
		t.Fatalf("seal original: %v", err)
	}
	if _, err := a.blobs.Remove(asset.SHA256, asset.Ext); err != nil {
		t.Fatalf("remove plaintext: %v", err)
	}
	if _, err := a.store.Pool().Exec(ctx,
		`update assets set vault = $2, vaulted_at = now(), ext = '', original_filename = ''
		 where id = $1::uuid`, asset.ID, bucket); err != nil {
		t.Fatalf("mark vaulted: %v", err)
	}
}

func TestVerifyIsQuietAboutAHiddenPhotograph(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0001.HEIC", []byte("a photograph worth hiding"),
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	a.seal(t, asset, db.VaultHidden)

	report := a.run(t, verify.Options{})
	for _, f := range report.Findings {
		t.Errorf("unexpected finding %s: %s", f.Kind, f.Detail)
	}
	if report.Critical() {
		t.Error("a hidden photograph was reported as a missing original")
	}
}

// The other direction: a sealed original that is genuinely gone is Critical,
// and for a worse reason than a missing blob — nothing else in the world holds
// a copy of it in this form.
func TestVerifyReportsASealedOriginalThatIsGone(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0002.HEIC", []byte("a photograph worth hiding"),
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	a.seal(t, asset, db.VaultArchive)

	if _, err := a.vaultBlobs.Remove(asset.SHA256, ""); err != nil {
		t.Fatalf("remove the sealed original: %v", err)
	}

	report := a.run(t, verify.Options{})
	found := false
	for _, f := range report.Findings {
		if f.Kind == verify.VaultMissing && strings.Contains(f.Detail, "archive") {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %v, want one about the sealed original", report.Findings)
	}
	if !report.Critical() {
		t.Error("a lost sealed original is not Critical; nothing else holds a copy of it")
	}
}
