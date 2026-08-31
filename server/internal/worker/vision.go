package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// visionRetryDelay is how long a job waits after photo-ml went away mid-call.
//
// Short, because the gate below is what actually keeps the pool off a dead
// service — this only has to cover the one job that was in flight when it went,
// and holding that one for half an hour would leave a gap in the middle of a
// backfill for no reason.
const visionRetryDelay = time.Minute

// mlProbeInterval bounds how often the pool asks photo-ml whether it is there.
// Cached across workers, so raising VISION_CONCURRENCY does not raise the probe
// rate.
const mlProbeInterval = 15 * time.Second

// ingestLeaseTTL is how long photo-ml is asked to hold the ingest lease past
// each renewal, and ingestRenewInterval is how often the gate renews it.
//
// The gap between them is deliberate and is the whole content of the setting: a
// lease that outlives three missed renewals is one that survives a slow database
// and a busy machine, and one that expires ninety seconds after the last is one
// that gives the card back on its own when photod is killed mid-backfill. The
// alternative — a lease held until something explicitly releases it — makes a
// `kill -9` during a four-hour caption pass into a card nobody can use until
// somebody notices and restarts photo-ml.
const (
	ingestLeaseTTL      = 90 * time.Second
	ingestRenewInterval = 30 * time.Second
	defaultIngestRetry  = 15 * time.Minute
)

// mlKinds are the three passes that need the card. In no particular order here:
// the priority between them is jobs.ClaimInOrder's business and this is only
// ever asked whether any of them exist.
var mlKinds = []jobs.Kind{jobs.KindVision, jobs.KindOCR, jobs.KindDescribe}

// runVision is the one job in this worker that leaves the machine's own
// process tree: the ML renditions go out over loopback and 1152 numbers per
// frame come back.
//
// It reads files and writes rows, and does no arithmetic of its own. That
// division is ML_IMAGES.md §3 — Go decodes, Python does tensors — and it is
// what lets photo-ml run as a user with no read access to /mnt/photos at all.
func (r *Runner) runVision(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
	}
	switch {
	case vaulted(asset):
		// The same silent success the other jobs give a vaulted asset, and here
		// it is the case with the least room for argument. An embedding is a
		// description of what a photograph looks like that a stranger can
		// search — it is the vault's whole objection, in 1152 numbers. The
		// renditions this would have read were sealed on the way in, so there
		// is nothing on disk to send either.
		return nil
	case asset.DeletedAt != nil:
		return nil
	case asset.IsOverlay, asset.IsLivePair():
		// Components of other assets' pictures rather than pictures. mlprep
		// already declined to write renditions for these; this repeats the
		// judgement rather than inferring it from the absence of files, so the
		// two jobs cannot drift into disagreeing about what an item is.
		return nil
	}

	frames, images, err := r.renditions(asset)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		// Nothing to look at, and not a failure. A video that could not be
		// sampled has no renditions — clipRenditions tolerates that on purpose,
		// because parking the job would mark a perfectly good archived video as
		// broken over a question nobody has asked yet. The same tolerance has
		// to reach this end of the pipe, or the leniency upstream just moves
		// the failure one job to the right.
		r.log().Debug("no ML renditions to embed; the asset will not be searchable by what it shows",
			"asset", asset.ID, "kind", asset.MediaKind)
		return nil
	}

	result, err := r.ML.EmbedImages(ctx, images)
	if err != nil {
		return err
	}
	if result.Dim != db.VisionDim {
		return fmt.Errorf("photo-ml returned %d-dimension vectors; asset_embeddings holds %d", result.Dim, db.VisionDim)
	}
	r.checkModel(&r.wrongModel, "encoder", result.Model, db.VisionModel)

	embeddings := make([]db.Embedding, len(frames))
	for i, frame := range frames {
		embeddings[i] = db.Embedding{Frame: frame, Vector: result.Vectors[i]}
	}
	return r.Store.PutEmbeddings(ctx, asset.ID, result.Model, embeddings)
}

// renditions reads what mlprep left on disk, and reports which frame each one
// is.
//
// The frame numbers are read off the filenames rather than assumed to be
// 0..n-1, because they are the coordinate the row is keyed by and a video whose
// renditions were rebuilt shorter has a gap at the end rather than a
// renumbering. A still is frame 0 and nothing else.
func (r *Runner) renditions(asset db.Asset) (frames []int, images [][]byte, err error) {
	read := func(frame int, suffix string) error {
		data, err := os.ReadFile(r.Derivatives.Path(asset.SHA256, suffix))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read ML rendition %s: %w", suffix, err)
		}
		if len(data) == 0 {
			return nil
		}
		frames = append(frames, frame)
		images = append(images, data)
		return nil
	}

	if asset.MediaKind == db.MediaVideo {
		for frame := range derivstore.MLFrameCount {
			if err := read(frame, derivstore.MLFrameSuffix(frame)); err != nil {
				return nil, nil, err
			}
		}
		return frames, images, nil
	}
	if err := read(0, derivstore.MLStill); err != nil {
		return nil, nil, err
	}
	return frames, images, nil
}

// checkModel says something, once, when photo-ml is not running the model this
// database expects.
//
// Never an error. Every one of these rows records what actually produced its
// contents, and storing that truthfully is more useful than refusing it — a
// second model's output sitting beside the first is exactly what makes a bench
// a delete and a requeue rather than a project.
//
// But it is worth saying out loud, because the consequences are quiet ones. The
// HNSW index in migration 0017 names db.VisionModel in its predicate, so
// vectors written under any other name are correct, searchable, and reached by
// sequential scan — which looks like the model being slow. Captions and
// recognised text written under another name are correct and simply never
// reach the tsvector, because rebuild_asset_search reads the model it was told
// about — which looks like the captioner having missed the photograph.
func (r *Runner) checkModel(once *sync.Once, role, reported, expected string) {
	if reported == expected {
		return
	}
	once.Do(func() {
		r.log().Warn("photo-ml is running a different "+role+" from the one this database expects",
			"reported", reported, "expected", expected,
			"consequence", "the rows are written truthfully and are not read back by search",
			"fix", "point photo-ml back at "+expected+", or make "+reported+" the name this archive uses")
	})
}

// mlAvailable is the vision pool's gate: three questions, in the order that
// makes the cheap ones able to answer for the expensive ones.
//
//	is there anything to do   one index probe, and the only one asked every tick
//	is photo-ml there         cached for mlProbeInterval, across every worker
//	may we have the card      the lease, and the reason this is not one question
//
// A pool that claimed first and discovered second would work — Defer exists for
// the job that is in flight when the service goes — but it would spend a claim,
// a lease and a heartbeat goroutine per asset to find out something that is
// true of the whole queue at once. Asking first means a machine whose GPU
// service is down, or whose card is being used by somebody, has a vision pool
// that is genuinely idle rather than one churning through sixty thousand rows.
//
// The first question is not cached, and that is worth saying because the other
// two are. It is one index probe, it is what decides whether the card is handed
// back, and caching it would mean a photograph that landed a second after the
// queue drained waited fifteen seconds to be looked at — the exact latency the
// nudge exists to remove.
func (r *Runner) mlAvailable(ctx context.Context) bool {
	if r.ML == nil {
		return false
	}

	r.mlMu.Lock()
	defer r.mlMu.Unlock()

	if !r.mlWork(ctx) {
		// Nothing queued and nothing running. The card goes back now rather
		// than when the lease lapses, because photod is the only process that
		// knows a queue is empty and the difference is whether the next person
		// to open the gallery gets a search box or a page of words. See
		// photo_ml/leases.py.
		r.releaseIngestLocked(ctx)
		return false
	}
	if !r.mlHealthyLocked(ctx) {
		return false
	}
	return r.mlLeaseLocked(ctx)
}

// mlWork reports whether any of the three GPU passes has work outstanding.
//
// Running counts as well as pending: a worker mid-caption is exactly when the
// lease must not be handed back, and Quiet already answers both in one query.
func (r *Runner) mlWork(ctx context.Context) bool {
	quiet, err := r.Queue.Quiet(ctx, mlKinds, 0)
	if err != nil {
		if ctx.Err() == nil {
			r.log().Error("ask whether there is any ML work outstanding", "error", err)
		}
		// Assume there is. A database that will not answer is not a reason to
		// hand back a lease that a caption pass may be halfway through, and the
		// claim below is about to fail against the same database anyway.
		return true
	}
	return !quiet
}

// mlHealthyLocked is the old gate: is the service there, and is it in a state to
// be given work.
//
// `ready` rather than `ok`: photo-ml answers /health from its first moment, on
// purpose, so that a service which is up and a service which is gone are
// distinguishable. What `ready` means changed when the search models stopped
// being loaded at startup — it used to be "the resident checkpoints are in
// VRAM" and is now "at least one model here can load at all" — but what the
// pool does about it did not.
func (r *Runner) mlHealthyLocked(ctx context.Context) bool {
	if time.Since(r.mlCheckedAt) < mlProbeInterval {
		return r.mlOK
	}

	health, err := r.ML.Health(ctx)
	if ctx.Err() != nil {
		return false
	}
	r.mlCheckedAt = time.Now()

	was := r.mlOK
	r.mlOK = err == nil && health.Ready

	// Logged on the edges only. A service that is down for an hour should be
	// two lines in the journal, not two hundred and forty.
	switch {
	case r.mlOK && !was:
		r.log().Info("photo-ml is answering; the vision pool is asking for the card",
			"device", health.Device, "dtype", health.Dtype, "models", residentNames(health))
	case !r.mlOK && was:
		reason := "not ready"
		if err != nil {
			reason = err.Error()
		}
		r.log().Warn("photo-ml is not answering; the vision pool is idle and no photographs are being described",
			"reason", reason, "note", "backups, the gallery and every other pool are unaffected")
	}
	return r.mlOK
}

// mlLeaseLocked takes or renews the ingest lease, and is the new half of this
// gate.
//
// The old arrangement had exactly one question about the card — is the service
// up — because the answer to every other one was fixed: the search models were
// loaded at startup and never given back, so an ingestion pass ran whenever
// there was work and shared a 16GB card with 3GB it could not displace. This is
// what replaced that. photo-ml decides; both facts it decides on are things
// this side cannot see (what the driver says is free, what else is loaded) and
// both facts it needs are things only this side can see (that there is a queue,
// and that it is still draining).
//
// A refusal is ordinary and is not an error. Somebody has the gallery open, or
// something outside this archive is holding most of the card. The queue is the
// state and the work is still in it, so the answer is to ask again in fifteen
// minutes — which is roughly how long the two things it is waiting on take to
// change, and is not a backoff: it does not grow, because nothing here is
// failing.
func (r *Runner) mlLeaseLocked(ctx context.Context) bool {
	now := time.Now()
	if r.holdingIngest {
		if now.Sub(r.ingestRenewedAt) < ingestRenewInterval {
			return true
		}
		grant, err := r.ML.Lease(ctx, mlclient.LeaseIngest, ingestLeaseTTL)
		if err == nil && grant.Held {
			r.ingestRenewedAt = now
			return true
		}
		// A renewal that was refused means the term lapsed and something else
		// took the card while this worker was between jobs. Nothing is wrong
		// and nothing is lost — the job that is running finishes, and the next
		// pass of this gate goes round the front and asks properly.
		r.holdingIngest = false
		if err != nil && ctx.Err() == nil {
			r.log().Warn("could not renew the ingest lease", "error", err)
		}
		return false
	}

	if !r.ingestRefusedAt.IsZero() && now.Sub(r.ingestRefusedAt) < r.ingestRetry() {
		return false
	}

	grant, err := r.ML.Lease(ctx, mlclient.LeaseIngest, ingestLeaseTTL)
	switch {
	case err != nil && !errors.Is(err, mlclient.ErrUnavailable):
		// A 4xx: this photo-ml does not arbitrate leases at all. An older
		// build, or one whose lease group this photod does not know about.
		// Treated as consent, because the alternative is a backfill that never
		// runs again over a version skew — and the service's own residency
		// rules, which are what this replaced, are still in place.
		r.holdingIngest = true
		r.ingestRenewedAt = now
		r.noLeases.Do(func() {
			r.log().Info("photo-ml does not arbitrate leases; the vision pool is claiming work without one",
				"reason", err.Error())
		})
		return true
	case err != nil:
		// Unreachable. The health probe above will say so on its next pass;
		// there is nothing useful to add here.
		return false
	case !grant.Held:
		r.ingestRefusedAt = now
		if grant.Reason != r.ingestRefusal {
			r.ingestRefusal = grant.Reason
			r.log().Info("photo-ml will not give the vision pool the card; the queued work is waiting",
				"reason", grant.Reason, "retry_in", r.ingestRetry())
		}
		return false
	}

	r.holdingIngest = true
	r.ingestRenewedAt = now
	r.ingestRefusedAt = time.Time{}
	r.ingestRefusal = ""
	r.log().Info("photo-ml gave the vision pool the card", "reason", grant.Reason)
	return true
}

// releaseIngest hands the card back. Safe to call when nothing is held.
func (r *Runner) releaseIngest(ctx context.Context) {
	r.mlMu.Lock()
	defer r.mlMu.Unlock()
	r.releaseIngestLocked(ctx)
}

func (r *Runner) releaseIngestLocked(ctx context.Context) {
	if !r.holdingIngest || r.ML == nil {
		return
	}
	r.holdingIngest = false
	if _, err := r.ML.ReleaseLease(ctx, mlclient.LeaseIngest); err != nil {
		// Not worth more than a debug line. The term is ninety seconds and it
		// is about to run out on its own, which is precisely what the term is
		// for; this call only makes the card available sooner.
		r.log().Debug("could not hand the ingest lease back", "error", err)
		return
	}
	r.log().Info("the ML queue is empty; the card is back for the gallery")
}

func (r *Runner) ingestRetry() time.Duration {
	if r.IngestRetry <= 0 {
		return defaultIngestRetry
	}
	return r.IngestRetry
}

func residentNames(h mlclient.Health) string {
	names := ""
	for _, m := range h.Models {
		if !m.Resident {
			continue
		}
		if names != "" {
			names += ","
		}
		names += m.Model
	}
	return names
}

// reconcileVision queues the embedding backfill.
//
// Only when photo-ml is configured, which is what keeps the jobs table honest
// on a machine that has no GPU service: no rows are written for work nothing
// can do, and the status page does not grow a permanent seventeen-thousand-item
// backlog to describe a feature that was never installed. Setting ML_URL and
// restarting is what turns the whole library into queued work, in one place.
func (r *Runner) reconcileVision(ctx context.Context) error {
	if r.ML == nil {
		return nil
	}
	n, err := jobs.ReconcileVision(ctx, r.Store.Pool(), db.VisionModel)
	if err != nil {
		return fmt.Errorf("reconcile vision: %w", err)
	}
	if n > 0 {
		r.log().Info("queued an embedding pass for assets that had no vector from this model",
			"assets", n, "model", db.VisionModel)
	}
	return nil
}
