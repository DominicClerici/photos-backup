package verify_test

import (
	"context"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

// dropIndex empties the database while leaving the archive untouched, which is
// the disaster this whole path exists for.
func (a *archive) dropIndex(t *testing.T) {
	t.Helper()
	if _, err := a.store.Pool().Exec(context.Background(),
		"truncate table assets, device_assets, jobs"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
}

// The guarantee in PROJECT.md's failure table: "Database lost → rebuilt by
// replaying manifest.jsonl". Until now that row was a claim.
func TestReindexRebuildsALostDatabase(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()

	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), captured)
	a.add(t, "IMG_7266", fixture(t, "iphone-portrait.heic"), captured)

	before, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	a.dropIndex(t)

	result, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if result.Inserted != 3 {
		t.Errorf("inserted %d, want 3", result.Inserted)
	}

	after, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if after.Assets != before.Assets {
		t.Errorf("rebuilt %d assets, want %d", after.Assets, before.Assets)
	}
	if after.Bytes != before.Bytes {
		t.Errorf("rebuilt %s of bytes, want %s", byteCount(after.Bytes), byteCount(before.Bytes))
	}

	// And the archive verifies clean against its rebuilt index.
	if report := a.run(t, verify.Options{Deep: true}); len(report.Findings) != 0 {
		t.Errorf("findings after a rebuild: %v", report.Findings)
	}
}

// The rebuild has to restore the device mappings too. Without them the phone is
// told "unknown" for every asset it holds and re-hashes its whole library on the
// next run — recovering the archive but not the property that makes backing it
// up cheap.
func TestReindexRestoresDeviceMappings(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	a.dropIndex(t)

	if _, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{}); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	known, err := a.store.KnownMappings(ctx, "iphone-14-pro", []db.LocalRef{
		{LocalID: "local-IMG_0001.HEIC"},
	})
	if err != nil {
		t.Fatalf("known mappings: %v", err)
	}
	if known["local-IMG_0001.HEIC"] == "" {
		t.Error("the device mapping did not survive the rebuild")
	}
}

// Running it against a database that is merely incomplete must be safe, and
// running it twice must do nothing the first run did not.
func TestReindexIsIdempotent(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), captured)

	// Nothing was lost at all: every line should already be indexed.
	first, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if first.Inserted != 0 || first.Existing != 2 {
		t.Errorf("inserted %d / existing %d, want 0 / 2", first.Inserted, first.Existing)
	}

	second, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("second reindex: %v", err)
	}
	if second.Inserted != 0 {
		t.Errorf("a repeat rebuild inserted %d rows", second.Inserted)
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 2 {
		t.Errorf("database holds %d assets after two rebuilds, want 2", counts.Assets)
	}
}

// A rebuilt asset needs its derivatives regenerated, and the way that happens
// is the ordinary queue — the same path a fresh upload takes.
func TestReindexQueuesDerivativeWork(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	a.dropIndex(t)

	if _, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{}); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	var pending int
	err := a.store.Pool().QueryRow(ctx,
		`select count(*) from jobs where kind = 'metadata' and state = 'pending'`).Scan(&pending)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if pending != 1 {
		t.Errorf("%d pending metadata jobs after a rebuild, want 1", pending)
	}
}

// The crash-between-rename-and-append case: bytes in the tree that the log
// never recorded. They are still someone's photo.
func TestReindexAdoptsBlobsWithNoManifestLine(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()
	content := fixture(t, "clip.mov")

	// Straight into the blob tree, bypassing the manifest entirely.
	if _, err := a.blobs.Put(bytesOf(content), ".mov", blobExpect(content)); err != nil {
		t.Fatalf("put blob: %v", err)
	}

	result, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if result.Adopted != 1 {
		t.Fatalf("adopted %d, want 1", result.Adopted)
	}

	asset, err := a.store.AssetBySHA256(ctx, shaHex(content))
	if err != nil {
		t.Fatalf("load adopted asset: %v", err)
	}
	if asset.MediaKind != db.MediaVideo {
		t.Errorf("adopted asset classified as %q, want video", asset.MediaKind)
	}
	if asset.ByteSize != int64(len(content)) {
		t.Errorf("adopted %d bytes, want %d", asset.ByteSize, len(content))
	}
}

// Without --adopt-orphans the same blob is left alone, so a rebuild can be
// strictly manifest-driven when that is what is wanted.
func TestReindexLeavesOrphansAloneWhenNotAsked(t *testing.T) {
	a := newArchive(t)
	content := fixture(t, "clip.mov")
	if _, err := a.blobs.Put(bytesOf(content), ".mov", blobExpect(content)); err != nil {
		t.Fatalf("put blob: %v", err)
	}

	result, err := verify.Reindex(context.Background(), a.deps, verify.ReindexOptions{AdoptOrphans: false})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if result.Adopted != 0 {
		t.Errorf("adopted %d without being asked", result.Adopted)
	}
}

func TestReindexDryRunTouchesNothing(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	a.dropIndex(t)

	result, err := verify.Reindex(ctx, a.deps, verify.ReindexOptions{DryRun: true, AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("dry run reported %d insertions, want 1", result.Inserted)
	}

	counts, err := a.store.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Assets != 0 {
		t.Errorf("a dry run inserted %d rows", counts.Assets)
	}
}

func TestReindexOnAnArchiveWithNoManifest(t *testing.T) {
	a := newArchive(t)

	result, err := verify.Reindex(context.Background(), a.deps, verify.ReindexOptions{AdoptOrphans: true})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if result.Lines != 0 || result.Inserted != 0 {
		t.Errorf("read %d lines and inserted %d from an archive with no manifest", result.Lines, result.Inserted)
	}
}
