package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// runMLPrep writes the renditions a vision model reads: one uncropped WebP for
// a photograph, several sampled across a video.
//
// The fourth kind of derivative this worker builds, and the one that exists so
// that a Python service never has to open the archive. photo-ml is handed image
// bytes over loopback; it holds no state, opens no files, and talks to no
// database, which is what makes the vault excluded by construction rather than
// by a WHERE clause somebody can forget to write. See ML_IMAGES.md §3.
//
// Queued at the end of the metadata job rather than beside it, for the reason
// the signature is: a video cannot be sampled without knowing how long it is,
// and that number is written by the job in front of this one.
func (r *Runner) runMLPrep(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
	}
	switch {
	case vaulted(asset):
		// The same silent success the other jobs give a vaulted asset, and here
		// it is the strongest case of the four. This job's whole output is a
		// legible 512px picture of the photograph, sitting unencrypted on the
		// SSD, addressed by a digest — which is a copy of the thing the vault
		// was asked to hide. See internal/vault.
		return nil
	case asset.DeletedAt != nil:
		// Moved to the trash between the queueing and the claim. Nothing is
		// going to search a deleted photograph, and the purge would delete the
		// renditions this wrote anyway.
		return nil
	case asset.IsOverlay, asset.IsLivePair():
		// Components of other assets' pictures rather than pictures. Both are
		// absent from every timeline, and indexing either would put a second
		// copy of one moment into every result page: a Live Photo's motion is a
		// near-duplicate of its own still, and a caption layer is one layer of
		// a memory that is already indexed as a composite.
		return nil
	}

	src := r.Blobs.Path(asset.SHA256, asset.Ext)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("original missing from the blob store: %w", err)
	}

	if asset.MediaKind == db.MediaVideo {
		return r.clipRenditions(ctx, asset, src)
	}
	return r.stillRendition(ctx, asset, src)
}

// stillRendition renders one photograph, through its caption layer if it has
// one, because for a memory the composite is the picture.
func (r *Runner) stillRendition(ctx context.Context, asset db.Asset, src string) error {
	overlay, err := r.overlay(ctx, asset)
	if err != nil {
		return err
	}
	if overlay != nil {
		overlay.Width, overlay.Height = intOr(asset.Width), intOr(asset.Height)
	}

	staged, cleanup, err := r.Derivatives.Stage("ml-*.webp")
	if err != nil {
		return err
	}
	defer cleanup()

	if err := r.Images.MLRendition(ctx, src, overlay, derivstore.MLEdge, staged); err != nil {
		return err
	}
	return r.Derivatives.Commit(asset.SHA256, derivstore.MLStill, staged)
}

// clipRenditions samples a video into derivstore.MLFrameCount stills.
//
// A clip that cannot be sampled is not a failure, which is the same tolerance
// clipSignature applies and for a stronger reason: parking the job would mark a
// perfectly good archived video as broken over a question — what is this a video
// of — that nobody has asked yet and that the rest of the archive does not
// depend on the answer to. The clip simply has nothing for the model to look
// at, and says so in the log.
//
// The frames are committed together, after all of them have been written. A run
// that published three of six and then died would leave a video the vision pass
// would describe by half of itself, with a job marked done behind it.
func (r *Runner) clipRenditions(ctx context.Context, asset db.Asset, src string) error {
	if r.Video == nil {
		return nil
	}
	info, err := r.Video.Probe(ctx, src)
	if err != nil {
		r.log().Warn("could not probe a video to sample it; it will not be searchable by what it shows",
			"asset", asset.ID, "error", err)
		return nil
	}

	// Burned in, unlike the duplicate scan's frames: a caption is text, and the
	// text recognition pass downstream is exactly what would read it.
	burned := ""
	if overlay, err := r.overlay(ctx, asset); err != nil {
		return err
	} else if overlay != nil {
		burned = overlay.Path
	}

	dir, cleanup, err := r.Derivatives.StageDir("mlframes-*")
	if err != nil {
		return err
	}
	defer cleanup()

	frames, err := r.Video.SampleImages(ctx, src, dir, burned, derivstore.MLFrameCount, derivstore.MLEdge, info)
	if err != nil {
		r.log().Warn("could not sample a video; it will not be searchable by what it shows",
			"asset", asset.ID, "error", err)
		return nil
	}

	for i, frame := range frames {
		if err := r.Derivatives.Commit(asset.SHA256, derivstore.MLFrameSuffix(i), frame); err != nil {
			return err
		}
	}
	// A clip that yielded fewer frames than asked for — one shorter than the
	// sampling interval — leaves stale renditions behind from a longer previous
	// run. Removing them is what keeps "frame N exists" and "frame N is of this
	// video" the same statement.
	for i := len(frames); i < derivstore.MLFrameCount; i++ {
		if err := r.Derivatives.Remove(asset.SHA256, derivstore.MLFrameSuffix(i)); err != nil {
			return err
		}
	}
	return nil
}

// reconcileMLPrep queues the renditions for everything visible that has none.
//
// Unlike the signature reconcile this finds work exactly once per asset: there
// is no version to compare, so a second start over the same library queues
// nothing. It is here rather than in a migration because the backfill is an
// hour of CPU that nothing is waiting for, and a migration that queued 17,788
// jobs would be a schema change that starts an hour of work.
func (r *Runner) reconcileMLPrep(ctx context.Context) error {
	n, err := jobs.ReconcileMLPrep(ctx, r.Store.Pool())
	if err != nil {
		return fmt.Errorf("reconcile mlprep: %w", err)
	}
	if n > 0 {
		r.log().Info("queued ML renditions for assets that had none", "assets", n)
	}
	return nil
}
