package verify_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

var captured = time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)

func TestIntactArchiveHasNoFindings(t *testing.T) {
	a := newArchive(t)
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), captured)

	report := a.run(t, verify.Options{Deep: true})

	if len(report.Findings) != 0 {
		t.Errorf("findings on an intact archive: %v", report.Findings)
	}
	if report.Checked != 2 {
		t.Errorf("checked %d assets, want 2", report.Checked)
	}
	if report.Hashed == 0 {
		t.Error("a deep run hashed nothing")
	}
	if report.Critical() {
		t.Error("an intact archive reported critical")
	}
}

// The finding that means a photo is gone. It is critical, and --fix must not
// pretend otherwise.
func TestMissingBlobIsCriticalAndNotFixable(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	if err := os.Remove(a.blobPath(asset)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	report := a.run(t, verify.Options{Fix: true})

	found := one(t, report, verify.BlobMissing)
	if found.Fixed {
		t.Error("--fix claimed to repair a missing original")
	}
	if !report.Critical() {
		t.Error("a missing original is not critical")
	}

	// Two passes notice the same loss from opposite directions, and both should
	// say so: the database's view and the manifest's view are independent
	// evidence, and a rebuild would need to know the log is wrong too.
	if got := findings(report, verify.ManifestOrphan); len(got) != 1 {
		t.Errorf("got %d manifest-orphan findings, want 1: %v", len(got), report.Findings)
	}
}

// Bit rot: the bytes changed without the name changing. Only a deep run sees
// it, and nothing is allowed to "repair" it.
func TestCorruptBlobIsFoundOnlyByDeepAndNeverRepaired(t *testing.T) {
	a := newArchive(t)
	content := fixture(t, "sample.heic")
	asset := a.add(t, "IMG_0001.HEIC", content, captured)

	// Flip a byte in place, keeping the length, so only a hash catches it.
	path := a.blobPath(asset)
	rotted := make([]byte, len(content))
	copy(rotted, content)
	rotted[len(rotted)/2] ^= 0xFF
	if err := os.WriteFile(path, rotted, 0o644); err != nil {
		t.Fatalf("rewrite blob: %v", err)
	}

	shallow := a.run(t, verify.Options{})
	if len(shallow.Findings) != 0 {
		t.Errorf("a shallow run should not read file contents, but found: %v", shallow.Findings)
	}

	deep := a.run(t, verify.Options{Deep: true, Fix: true})
	found := only(t, deep, verify.BlobCorrupt)
	if found.Fixed {
		t.Error("--fix claimed to repair bit rot")
	}
	if !deep.Critical() {
		t.Error("bit rot is not critical")
	}
}

// A truncated file is caught without hashing, which is what makes the cheap
// nightly run worth anything.
func TestWrongSizeIsFoundWithoutHashing(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	if err := os.Truncate(a.blobPath(asset), 128); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}

	report := a.run(t, verify.Options{})

	only(t, report, verify.BlobWrongSize)
	if report.Hashed != 0 {
		t.Errorf("hashed %d bytes on a shallow run", report.Hashed)
	}
	if !report.Critical() {
		t.Error("a truncated original is not critical")
	}
}

// The known gap in the commit ordering: a crash between the rename and the
// manifest append. The database still holds everything the line needs, so this
// is the one finding --fix resolves completely.
func TestMissingManifestLineIsRepaired(t *testing.T) {
	a := newArchive(t)
	content := fixture(t, "sample.heic")
	asset := a.add(t, "IMG_0001.HEIC", content, captured)

	if err := os.Remove(a.manifestAt); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	report := a.run(t, verify.Options{})
	found := only(t, report, verify.ManifestMissing)
	if found.Fixed {
		t.Error("a read-only run repaired something")
	}
	if report.Critical() {
		t.Error("a missing manifest line is not a lost photo")
	}

	fixed := a.run(t, verify.Options{Fix: true})
	if got := only(t, fixed, verify.ManifestMissing); !got.Fixed {
		t.Fatal("--fix did not restore the manifest line")
	}

	entries, err := manifest.Read(a.manifestAt)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("manifest holds %d lines, want 1", len(entries))
	}
	if entries[0].SHA256 != asset.SHA256 || entries[0].Filename != "IMG_0001.HEIC" {
		t.Errorf("restored line is wrong: %+v", entries[0])
	}
	if entries[0].Ext != ".heic" || entries[0].ContentType != "image/heic" {
		t.Errorf("restored line lost its classification: %+v", entries[0])
	}

	// And the archive is clean afterwards, which is the actual point.
	if after := a.run(t, verify.Options{}); len(after.Findings) != 0 {
		t.Errorf("findings after repair: %v", after.Findings)
	}
}

// A manifest line whose blob is gone means the log describes something the
// archive no longer has.
func TestManifestOrphanIsCritical(t *testing.T) {
	a := newArchive(t)
	content := fixture(t, "sample.heic")

	if err := a.manifest.Append(manifest.Entry{
		SHA256: shaHex(content), MD5: md5Hex(content), Size: int64(len(content)),
		Filename: "GONE.HEIC", ContentType: "image/heic", Ext: ".heic",
		StoredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append manifest: %v", err)
	}

	report := a.run(t, verify.Options{})

	only(t, report, verify.ManifestOrphan)
	if !report.Critical() {
		t.Error("a manifest line with no blob is not critical")
	}
}

// The lost-database direction: the bytes are safe and nothing knows about them.
func TestUnindexedBlobIsReported(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	if _, err := a.store.Pool().Exec(context.Background(),
		"delete from assets where id = $1", asset.ID); err != nil {
		t.Fatalf("delete row: %v", err)
	}

	report := a.run(t, verify.Options{})

	kinds := map[verify.Kind]int{}
	for _, f := range report.Findings {
		kinds[f.Kind]++
	}
	if kinds[verify.BlobUnindexed] != 1 {
		t.Errorf("got %d unindexed findings, want 1: %v", kinds[verify.BlobUnindexed], report.Findings)
	}
	if report.Critical() {
		t.Error("an unindexed blob is not a lost photo — the bytes are right there")
	}
}

// A derivative is rebuildable, so the repair is to queue the job that rebuilds
// it rather than to touch any file.
func TestMissingDerivativeIsRequeued(t *testing.T) {
	a := newArchive(t)
	ctx := context.Background()
	asset := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	if err := a.derivs.Write(asset.SHA256, derivstore.Thumb, func(w io.Writer) error {
		_, err := w.Write([]byte("a thumbnail"))
		return err
	}); err != nil {
		t.Fatalf("write thumb: %v", err)
	}
	if err := a.store.SetDerivedState(ctx, asset.ID, db.DerivedReady); err != nil {
		t.Fatalf("set derived state: %v", err)
	}
	if clean := a.run(t, verify.Options{}); len(clean.Findings) != 0 {
		t.Fatalf("findings before the derivative was removed: %v", clean.Findings)
	}

	if err := a.derivs.Remove(asset.SHA256, derivstore.Thumb); err != nil {
		t.Fatalf("remove thumb: %v", err)
	}

	report := a.run(t, verify.Options{Fix: true})
	found := only(t, report, verify.DerivativeMissing)
	if !found.Fixed {
		t.Error("--fix did not requeue the derivative")
	}

	var pending int
	err := a.store.Pool().QueryRow(ctx,
		`select count(*) from jobs where asset_id = $1 and kind = 'metadata' and state = 'pending'`,
		asset.ID).Scan(&pending)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if pending != 1 {
		t.Errorf("%d pending metadata jobs, want 1", pending)
	}
}

// Litter costs disk, not photos. It is swept only once it is old enough that it
// cannot be an upload still in flight.
func TestStaleUploadsAreSweptAndFreshOnesAreLeft(t *testing.T) {
	a := newArchive(t)
	incoming := filepath.Join(a.root, "incoming")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatalf("mkdir incoming: %v", err)
	}

	stale := filepath.Join(incoming, "0123456789abcdef0123456789abcdef.part")
	fresh := filepath.Join(incoming, "fedcba9876543210fedcba9876543210.part")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	report := a.run(t, verify.Options{Fix: true, StaleAfter: 24 * time.Hour})

	found := only(t, report, verify.StaleUpload)
	if !found.Fixed {
		t.Error("--fix did not sweep the stale partial")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale partial survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("an upload that could still be in flight was swept")
	}
}

// --fix reports a parked job and leaves it parked; --retry-failed is what puts
// it back. The split exists because the weekly timer runs --fix, and a job that
// already spent every attempt would otherwise be reground forever.
func TestRetryFailedRequeuesAParkedJobAndFixDoesNot(t *testing.T) {
	ctx := context.Background()
	a := newArchive(t)
	asset := a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))

	if err := jobs.Enqueue(ctx, a.deps.Store.Pool(), jobs.KindMetadata, asset.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Parked directly. How a job spends its attempts is the queue's business;
	// what is under test is what verify does once one has.
	if _, err := a.deps.Store.Pool().Exec(ctx,
		`update jobs set state = 'failed', attempts = $1, last_error = $2 where asset_id = $3::uuid`,
		jobs.DefaultMaxAttempts, "no frame was written", asset.ID); err != nil {
		t.Fatalf("park the job: %v", err)
	}

	fixed := a.run(t, verify.Options{Fix: true})
	if f := one(t, fixed, verify.DerivativeFailed); f.Fixed {
		t.Error("--fix requeued a parked job; that belongs to --retry-failed")
	}

	retried := a.run(t, verify.Options{RetryFailed: true})
	if f := one(t, retried, verify.DerivativeFailed); !f.Fixed {
		t.Fatal("--retry-failed did not requeue the parked job")
	}
	if _, err := a.deps.Queue.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "t"); err != nil {
		t.Errorf("the requeued job was not claimable: %v", err)
	}
}

func TestEmptyArchiveIsClean(t *testing.T) {
	a := newArchive(t)

	report := a.run(t, verify.Options{Deep: true})

	if len(report.Findings) != 0 {
		t.Errorf("findings on an empty archive: %v", report.Findings)
	}
	if report.Critical() {
		t.Error("an empty archive reported critical")
	}
}
