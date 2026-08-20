package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

// Scan looks for everything that ought to be one item and is several.
//
// Both halves in one pass, in this order, because the second depends on the
// first: a recording exported in six pieces is six rows that look alike, and
// offering them as duplicates would be asking somebody to approve throwing away
// five sixths of a minute of video. Finding the segment groups first puts those
// rows behind the exclusion in db.SignaturesForScan, so the duplicate sweep
// never sees them.
//
// It is deliberately not on a timer of its own. The two things that create work
// here are an import and a backfill, both of which end, and a sweep that ran
// hourly over a library nobody had touched would burn three seconds of every
// hour to write nothing. Callers: startup, the end of a `photobackup import`,
// and the button on the review page.
func (r *Runner) Scan(ctx context.Context) (merge.ScanResult, error) {
	started := time.Now()
	var out merge.ScanResult

	candidates, err := r.Store.SegmentCandidates(ctx)
	if err != nil {
		return out, err
	}
	if out.Segments, err = r.Store.RecordGroups(ctx, merge.Segments(candidates)); err != nil {
		return out, err
	}
	if out.Queued, err = r.EnqueueSegmentMerges(ctx); err != nil {
		return out, err
	}

	coverage, err := r.Store.SignatureCoverage(ctx)
	if err != nil {
		return out, err
	}
	out.Signed, out.Assets = coverage.Signed, coverage.Assets

	signatures, err := r.Store.SignaturesForScan(ctx)
	if err != nil {
		return out, err
	}
	blocked, err := r.Store.BlockedPairs(ctx)
	if err != nil {
		return out, err
	}
	groups := merge.Duplicates(signatures, blocked, merge.DefaultOptions())
	if out.Duplicates, err = r.Store.RecordGroups(ctx, groups); err != nil {
		return out, err
	}

	out.Took = time.Since(started)
	r.log().Info("scanned the library for things that should be one item",
		"segment_groups", out.Segments, "duplicate_groups", out.Duplicates,
		"joins_queued", out.Queued, "signatures", out.Signed, "assets", out.Assets,
		"took", out.Took)
	return out, nil
}

// reconcileSignatures queues the decode of every original whose signature is
// missing or stale, and is what a change to the hashing actually costs: one
// bump of merge.SignatureVersion, one restart, and an hour of one core.
func (r *Runner) reconcileSignatures(ctx context.Context) error {
	n, err := jobs.ReconcileSignatures(ctx, r.Store.Pool(), merge.SignatureVersion)
	if err != nil {
		return fmt.Errorf("reconcile signatures: %w", err)
	}
	if n > 0 {
		r.log().Info("queued signature work for assets that had none or had an old one",
			"assets", n, "version", merge.SignatureVersion)
	}
	return nil
}
