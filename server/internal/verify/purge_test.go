package verify_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/purge"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

func (a *archive) purger() purge.Deps {
	return purge.Deps{
		Store:       a.store,
		Blobs:       a.blobs,
		Derivatives: a.derivs,
		Manifest:    a.manifest,
		Log:         slog.New(slog.DiscardHandler),
	}
}

// The rebuild replays every line the archive ever wrote, and one of those lines
// now says a photograph was thrown away on purpose. Without this, the recovery
// path is also a resurrection path: everything ever deleted comes back the first
// time the database is rebuilt.
func TestReindexDoesNotResurrectPurgedAssets(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()

	kept := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	gone := a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), captured)

	if _, err := a.store.Trash(ctx, db.Selection{IDs: []string{gone.ID}}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	result, err := purge.Selection(ctx, a.purger(), db.Selection{IDs: []string{gone.ID}})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if result.Items != 1 {
		t.Fatalf("purged %d items, want 1", result.Items)
	}

	a.dropIndex(t)

	rebuilt, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if rebuilt.Inserted != 1 {
		t.Errorf("inserted %d, want 1: only the photograph that was kept", rebuilt.Inserted)
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 1 {
		t.Errorf("the rebuilt archive holds %d assets, want 1", counts.Assets)
	}
	if _, err := a.store.AssetBySHA256(ctx, kept.SHA256); err != nil {
		t.Errorf("the kept photograph did not survive the rebuild: %v", err)
	}

	// And the tombstone is back, so the phone that still holds it is told not
	// to offer it again. That row lives in the database, which is exactly what
	// was just lost, so the manifest is the only thing that could restore it.
	key := db.ContentKey{MD5: gone.MD5, ByteSize: gone.ByteSize}
	purged, err := a.store.PurgedContent(ctx, []db.ContentKey{key})
	if err != nil {
		t.Fatalf("purged content: %v", err)
	}
	if !purged[key] {
		t.Error("the rebuild lost the tombstone; the next backup would upload it again")
	}
}

// A blob whose unlink failed, or one restored from a backup taken before the
// purge, is still on disk with nothing pointing at it. The orphan pass exists to
// adopt exactly that — and it must not adopt this.
func TestReindexWillNotAdoptAPurgedBlobBackIn(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()

	gone := a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), captured)
	if _, err := a.store.Trash(ctx, db.Selection{IDs: []string{gone.ID}}); err != nil {
		t.Fatalf("trash: %v", err)
	}

	// Everything a purge does except remove the file, which is what a failed
	// unlink leaves behind.
	deps := a.purger()
	deps.Blobs = nil
	if _, err := purge.Selection(ctx, deps, db.Selection{IDs: []string{gone.ID}}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := a.blobs.Open(gone.SHA256, gone.Ext); err != nil {
		t.Fatalf("the fixture for this test needs the blob to still be there: %v", err)
	}

	a.dropIndex(t)

	rebuilt, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if rebuilt.Adopted != 0 {
		t.Errorf("adopted %d orphans, want 0", rebuilt.Adopted)
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 0 {
		t.Errorf("the rebuilt archive holds %d assets, want 0", counts.Assets)
	}
}

// Purging content and archiving it again later are both things somebody can do,
// and the log records them in the order they happened. The last line about a
// digest is the one that is true.
func TestReindexKeepsContentArchivedAfterItWasPurged(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()

	content := fixture(t, "sample.heic")
	first := a.add(t, "IMG_0001.HEIC", content, captured)

	if _, err := a.store.Trash(ctx, db.Selection{IDs: []string{first.ID}}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if _, err := purge.Selection(ctx, a.purger(), db.Selection{IDs: []string{first.ID}}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// The same bytes, archived again — which the sync path would refuse but a
	// direct upload or an import will happily do.
	a.add(t, "IMG_0001.HEIC", content, captured)

	a.dropIndex(t)

	if _, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 1 {
		t.Errorf("the rebuilt archive holds %d assets, want 1", counts.Assets)
	}
}
