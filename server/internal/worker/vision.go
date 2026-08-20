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

// mlAvailable reports whether photo-ml is up and warm, and is the reason the
// vision pool does not need an error path for "the service is not installed".
//
// A pool that claimed first and discovered second would work — Defer exists for
// the job that is in flight when the service goes — but it would spend a claim,
// a lease and a heartbeat goroutine per asset to find out something that is
// true of the whole queue at once. Asking first, and asking at most once every
// mlProbeInterval however many workers there are, means a machine whose GPU
// service is down has a vision pool that is genuinely idle rather than one
// churning through sixty thousand rows.
//
// `ready` rather than `ok`: photo-ml answers /health from its first moment, on
// purpose, so that warming up and being gone are distinguishable. Handing work
// to a service that is listening but still pulling weights off a mirror would
// only produce a timeout.
func (r *Runner) mlAvailable(ctx context.Context) bool {
	if r.ML == nil {
		return false
	}

	r.mlMu.Lock()
	defer r.mlMu.Unlock()
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
		r.log().Info("photo-ml is answering; the vision pool is claiming work",
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
