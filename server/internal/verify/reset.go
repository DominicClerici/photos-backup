package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResetOptions configures an erase.
type ResetOptions struct {
	// DryRun reports what would go without removing anything.
	DryRun bool
	// TLSDir is the certificate authority's directory, resolved the way photod
	// resolves it. Reset never touches it — it is named here so a run can refuse
	// when it sits inside something about to be removed. See checkTLSSafe.
	TLSDir string
}

// ResetTarget is one thing an erase removes.
type ResetTarget struct {
	Path string
	// What it holds, for the confirmation prompt.
	What string
	// Clear empties the directory and keeps the directory itself.
	Clear bool
	// Present is whether anything was there. Filled in by Reset.
	Present bool
}

// ResetResult is what an erase did.
type ResetResult struct {
	// Assets and Bytes are what the database held before it was emptied.
	Assets int64
	Bytes  int64
	// Devices is how many paired devices came through it, which is every one
	// that went in.
	Devices int
	// Targets is what was removed, or under DryRun what would be.
	Targets []ResetTarget
	Elapsed time.Duration
}

// Reset erases the archive and its index, and keeps paired devices working.
//
// It is the deliberate counterpart to Reindex. Reindex exists because the blob
// tree plus manifest.jsonl can rebuild a lost database; that same property means
// emptying the database alone would achieve nothing, since the next reindex
// would put every asset straight back. So all four stores go together:
//
//	blobs/           the originals
//	manifest.jsonl   the replay log that could resurrect them
//	incoming/        partial uploads, which reference nothing that still exists
//	derivatives      thumbnails and playback renditions
//
// What survives is the devices table and the CA, which between them are the
// entire reason a paired phone can still talk to the server afterwards.
//
// The order matters. The database is emptied first: a crash partway through the
// file removals then leaves bytes with no rows, which verify reports as blob
// orphans and a second run clears. The reverse order would leave rows pointing
// at originals that no longer exist — indistinguishable, to verify and to
// whoever reads its output, from the archive having been silently destroyed.
func Reset(ctx context.Context, d Deps, opt ResetOptions) (ResetResult, error) {
	started := time.Now()
	var result ResetResult

	targets, err := resetTargets(d)
	if err != nil {
		return result, err
	}
	if err := checkTLSSafe(targets, opt.TLSDir); err != nil {
		return result, err
	}

	counts, err := d.Store.Counts(ctx)
	if err != nil {
		return result, err
	}
	result.Assets, result.Bytes = counts.Assets, counts.Bytes

	devicesBefore, err := d.Store.CountDevices(ctx)
	if err != nil {
		return result, err
	}
	result.Devices = devicesBefore

	for i := range targets {
		targets[i].Present = hasContent(targets[i])
	}
	result.Targets = targets

	if opt.DryRun {
		result.Elapsed = time.Since(started)
		return result, nil
	}

	if err := d.Store.ResetLibrary(ctx); err != nil {
		return result, err
	}

	devicesAfter, err := d.Store.CountDevices(ctx)
	if err != nil {
		return result, err
	}
	if devicesAfter != devicesBefore {
		return result, fmt.Errorf(
			"reset removed %d of %d paired devices — this is a bug; re-pair the affected phones",
			devicesBefore-devicesAfter, devicesBefore)
	}

	for _, t := range targets {
		if err := removeTarget(t); err != nil {
			return result, err
		}
	}

	result.Elapsed = time.Since(started)
	return result, nil
}

// resetTargets is every place archived content lives, as absolute paths.
func resetTargets(d Deps) ([]ResetTarget, error) {
	root, err := filepath.Abs(d.PhotosRoot)
	if err != nil {
		return nil, err
	}
	uploadDir, err := filepath.Abs(d.Uploads.Dir())
	if err != nil {
		return nil, err
	}
	derivRoot, err := filepath.Abs(d.Derivatives.Root())
	if err != nil {
		return nil, err
	}

	return []ResetTarget{
		{Path: filepath.Join(root, "blobs"), What: "originals"},
		{Path: filepath.Join(root, "manifest.jsonl"), What: "the replay log"},
		{Path: uploadDir, What: "partial uploads"},
		// The derivatives root is emptied rather than removed. It is the one
		// target that is routinely a directory somebody else created — the
		// deployment puts it under /var/lib/photod, installed with an ownership
		// and mode that photod running as an unprivileged user would not
		// reproduce if it had to make the directory again.
		{Path: derivRoot, What: "thumbnails and playback renditions", Clear: true},
	}, nil
}

// checkTLSSafe refuses a reset that would take the certificate authority with
// it.
//
// TLS_DIR is unset by default, which puts ca.key at PhotosRoot/tls — inside the
// tree this command deletes from. Nothing in the default layout collides, but a
// DERIVATIVES_ROOT pointed at the photos root would empty it wholesale, and
// losing ca.key means re-pairing every phone. That is precisely the outcome
// reset exists to avoid, so it is worth a check rather than a warning.
func checkTLSSafe(targets []ResetTarget, tlsDir string) error {
	if tlsDir == "" {
		return nil
	}
	abs, err := filepath.Abs(tlsDir)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if within(t.Path, abs) {
			return fmt.Errorf(
				"refusing to reset: %s holds the CA and sits inside %s, which this would erase — "+
					"every paired device would have to be paired again. Move TLS_DIR outside the archive",
				abs, t.Path)
		}
	}
	return nil
}

// within reports whether child is parent or lies inside it.
//
// The ".." tests are separator-aware on purpose: a plain prefix check would read
// a directory legitimately named "..old" as an escape and answer false, and a
// false negative here is the direction that deletes the CA.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// hasContent reports whether a target holds anything worth mentioning. A
// cleared directory that is already empty counts as nothing, so a second reset
// says so instead of listing four paths it will not change.
func hasContent(t ResetTarget) bool {
	if !t.Clear {
		return exists(t.Path)
	}
	entries, err := os.ReadDir(t.Path)
	return err == nil && len(entries) > 0
}

func removeTarget(t ResetTarget) error {
	if t.Clear {
		return clearDir(t.Path)
	}
	if err := os.RemoveAll(t.Path); err != nil {
		return fmt.Errorf("remove %s: %w", t.Path, err)
	}
	return nil
}

// clearDir empties a directory, keeping the directory itself.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}
