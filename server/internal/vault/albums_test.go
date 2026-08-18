package vault

import (
	"context"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// Filing something that is already hidden.
//
// The album membership of a hidden photograph is inside its sealed document,
// so this is the one test that says the document can be rewritten without
// losing what else was in it — and that the rewrite survives a restore, which
// is the only thing that ever reads it back.
func TestAlbumMembershipGoesIntoTheSealedDocument(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 1, []byte("a photograph"))
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

	album, err := h.store.CreateAlbum(ctx, db.NewAlbum{Title: "Private", Vault: db.VaultHidden})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	ref := AlbumRef{ID: album.ID, Title: album.Title, Source: album.Source}

	added, err := h.SetAlbum(ctx, db.VaultHidden, []string{asset.ID}, ref, true)
	if err != nil {
		t.Fatalf("SetAlbum: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}

	item, err := h.Item(ctx, asset.ID)
	if err != nil {
		t.Fatalf("Item: %v", err)
	}
	if len(item.Doc.Albums) != 1 || item.Doc.Albums[0].ID != album.ID {
		t.Fatalf("sealed albums = %v, want just the new one", item.Doc.Albums)
	}
	// The rest of the document is untouched: the row it carries is what a
	// restore writes back, and a reseal that lost it would lose the photograph's
	// metadata for good.
	if item.Filename() != asset.OriginalFilename {
		t.Errorf("filename after resealing = %q, want %q", item.Filename(), asset.OriginalFilename)
	}

	// Doing it again changes nothing and says so, rather than re-encrypting
	// every document in a selection that was already where it was being put.
	again, err := h.SetAlbum(ctx, db.VaultHidden, []string{asset.ID}, ref, true)
	if err != nil {
		t.Fatalf("SetAlbum twice: %v", err)
	}
	if again != 0 {
		t.Errorf("re-adding reported %d, want 0", again)
	}

	removed, err := h.SetAlbum(ctx, db.VaultHidden, []string{asset.ID}, ref, false)
	if err != nil {
		t.Fatalf("SetAlbum remove: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	item, err = h.Item(ctx, asset.ID)
	if err != nil {
		t.Fatalf("Item after removal: %v", err)
	}
	if len(item.Doc.Albums) != 0 {
		t.Errorf("sealed albums = %v, want none", item.Doc.Albums)
	}
}

// A photograph filed while hidden rejoins that album when it comes back out —
// but only if the album has come out too. That is CommitUnvault's rule, and it
// applies to memberships written here exactly as it does to the ones that went
// in with the photograph.
func TestAnAlbumJoinedInTheVaultSurvivesTheRestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 1, []byte("a photograph"))
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

	album, err := h.store.CreateAlbum(ctx, db.NewAlbum{Title: "Kept", Vault: db.VaultArchive})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	if _, err := h.SetAlbum(ctx, db.VaultArchive, []string{asset.ID},
		AlbumRef{ID: album.ID, Title: album.Title}, true); err != nil {
		t.Fatalf("SetAlbum: %v", err)
	}

	// The album comes out first, exactly as handleVaultRestore orders it.
	if err := h.store.UnvaultAlbum(ctx, album.ID); err != nil {
		t.Fatalf("UnvaultAlbum: %v", err)
	}
	if _, err := h.Remove(ctx, []string{asset.ID}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	held, err := h.store.AlbumIDsOf(ctx, asset.ID)
	if err != nil {
		t.Fatalf("AlbumIDsOf: %v", err)
	}
	if len(held) != 1 || held[0] != album.ID {
		t.Errorf("albums after the restore = %v, want the one it joined while hidden", held)
	}
}

// A bucket is a boundary. An id from the other one is skipped rather than
// filed, so nothing can put an archived photograph into a hidden album.
func TestSetAlbumIgnoresTheOtherBucket(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 1, []byte("a photograph"))
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

	album, err := h.store.CreateAlbum(ctx, db.NewAlbum{Title: "Archived", Vault: db.VaultArchive})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}
	added, err := h.SetAlbum(ctx, db.VaultArchive, []string{asset.ID},
		AlbumRef{ID: album.ID, Title: album.Title}, true)
	if err != nil {
		t.Fatalf("SetAlbum: %v", err)
	}
	if added != 0 {
		t.Errorf("added = %d, want a hidden photo to be left out of an archived album", added)
	}
}

// The one operation in this feature that a locked vault cannot do, and the
// reason is not policy: the document has to be opened to be added to.
func TestSetAlbumNeedsThePassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	asset := h.archive(t, 1, []byte("a photograph"))
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
	album, err := h.store.CreateAlbum(ctx, db.NewAlbum{Title: "Private", Vault: db.VaultHidden})
	if err != nil {
		t.Fatalf("CreateAlbum: %v", err)
	}

	h.Keeper.Lock()
	if _, err := h.SetAlbum(ctx, db.VaultHidden, []string{asset.ID},
		AlbumRef{ID: album.ID, Title: album.Title}, true); err != ErrLocked {
		t.Fatalf("SetAlbum on a locked vault returned %v, want ErrLocked", err)
	}
}
