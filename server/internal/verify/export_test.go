package verify_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/verify"
)

// tree lists the export relative to its root, so a test can assert the shape of
// the whole thing at once.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk export: %v", err)
	}
	sort.Strings(out)
	return out
}

func TestExportBuildsADateTreeOfHardlinks(t *testing.T) {
	a := newArchive(t)
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), time.Date(2026, 3, 14, 9, 26, 0, 0, time.UTC))
	a.add(t, "IMG_0002.MOV", fixture(t, "clip.mov"), time.Date(2024, 12, 25, 18, 5, 0, 0, time.UTC))

	dest := t.TempDir()
	result, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{Dest: dest})
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	want := []string{
		"2024/2024-12-25/IMG_0002.MOV",
		"2026/2026-03-14/IMG_0001.HEIC",
	}
	got := tree(t, dest)
	if len(got) != len(want) {
		t.Fatalf("exported %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("exported %q, want %q", got[i], want[i])
		}
	}
	if result.Linked != 2 {
		t.Errorf("linked %d, want 2", result.Linked)
	}
	if result.Copied != 0 {
		t.Errorf("copied %d files during a hardlink export", result.Copied)
	}
}

// The promise of export is that it costs nothing. A hardlink shares an inode
// with the blob, which is how that is true.
func TestExportUsesNoAdditionalDiskSpace(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	dest := t.TempDir()
	if _, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{Dest: dest}); err != nil {
		t.Fatalf("export: %v", err)
	}

	blob, err := os.Stat(a.blobPath(asset))
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	exported, err := os.Stat(filepath.Join(dest, "2026", "2026-03-14", "IMG_0001.HEIC"))
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if !os.SameFile(blob, exported) {
		t.Error("the exported file is a copy, not a hardlink to the blob")
	}
}

// Phones reset their counters and two cameras both produce IMG_0001. A
// collision must never silently drop a photo.
func TestExportDisambiguatesCollidingFilenames(t *testing.T) {
	a := newArchive(t)
	day := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), day)
	a.add(t, "IMG_0001.HEIC", fixture(t, "iphone-portrait.heic"), day)

	dest := t.TempDir()
	result, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{Dest: dest})
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	got := tree(t, dest)
	if len(got) != 2 {
		t.Fatalf("exported %d files, want 2: %v", len(got), got)
	}
	if got[0] == got[1] {
		t.Fatalf("both photos exported to the same name: %v", got)
	}
	if result.Renamed != 1 {
		t.Errorf("renamed %d, want 1", result.Renamed)
	}
}

// A Takeout video arrives with no extension and is stored as .mov. The export
// should carry the extension the bytes actually justify, not the one the
// filename lacked.
func TestExportGivesSniffedFilesTheirRealExtension(t *testing.T) {
	a := newArchive(t)
	a.add(t, "IMG_7266", fixture(t, "clip.mov"), captured)

	dest := t.TempDir()
	if _, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{Dest: dest}); err != nil {
		t.Fatalf("export: %v", err)
	}

	got := tree(t, dest)
	if len(got) != 1 || got[0] != "2026/2026-03-14/IMG_7266.mov" {
		t.Errorf("exported %v, want 2026/2026-03-14/IMG_7266.mov", got)
	}
}

// Re-running an export should be free rather than an error, so it can be a
// cron job.
func TestExportIsIdempotent(t *testing.T) {
	a := newArchive(t)
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	dest := t.TempDir()
	opt := verify.ExportOptions{Dest: dest}
	if _, err := verify.Export(context.Background(), a.store, a.blobs, opt); err != nil {
		t.Fatalf("first export: %v", err)
	}

	second, err := verify.Export(context.Background(), a.store, a.blobs, opt)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if second.Skipped != 1 {
		t.Errorf("skipped %d on a repeat export, want 1", second.Skipped)
	}
	if got := tree(t, dest); len(got) != 1 {
		t.Errorf("repeat export produced %v", got)
	}
}

func TestExportRespectsADateRange(t *testing.T) {
	a := newArchive(t)
	a.add(t, "OLD.HEIC", fixture(t, "sample.heic"), time.Date(2015, 6, 1, 12, 0, 0, 0, time.UTC))
	a.add(t, "NEW.HEIC", fixture(t, "iphone-portrait.heic"), time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	dest := t.TempDir()
	_, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{
		Dest: dest,
		From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	got := tree(t, dest)
	if len(got) != 1 || got[0] != "2026/2026-06-01/NEW.HEIC" {
		t.Errorf("exported %v, want only the 2026 photo", got)
	}
}

func TestExportDryRunWritesNothing(t *testing.T) {
	a := newArchive(t)
	a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)

	dest := filepath.Join(t.TempDir(), "not-created")
	result, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{
		Dest: dest, DryRun: true,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.Linked != 1 {
		t.Errorf("dry run reported %d files, want 1", result.Linked)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a dry run created the destination")
	}
}

// An indexed original that is not on disk is reported, not fatal: export is not
// the right tool to discover that, and verify says it far better.
func TestExportReportsMissingOriginals(t *testing.T) {
	a := newArchive(t)
	asset := a.add(t, "IMG_0001.HEIC", fixture(t, "sample.heic"), captured)
	if err := os.Remove(a.blobPath(asset)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	result, err := verify.Export(context.Background(), a.store, a.blobs, verify.ExportOptions{Dest: t.TempDir()})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.Missing != 1 {
		t.Errorf("missing = %d, want 1", result.Missing)
	}
	if result.Linked != 0 {
		t.Errorf("linked %d files, want 0", result.Linked)
	}
}
