// Package worker runs the derivative pipeline. It is the only place that knows
// what happens to an asset and in what order; the queue below it knows nothing
// about images, and the media packages above it know nothing about queues.
//
// It runs two independent pools rather than one. A single shared pool lets a
// handful of 4K transcodes claim every slot, and every thumbnail queued behind
// them waits minutes for work that takes 80ms — so during a backfill the
// gallery would appear to be doing nothing at all.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/video"
)

// Deps are the collaborators a runner needs. They are all interfaces to their
// own packages' concrete types rather than mocks: the pipeline's job is to
// sequence real tools, and a test that stubs ffmpeg would verify the stub.
type Deps struct {
	Store       *db.Store
	Queue       *jobs.Queue
	Blobs       *blobstore.Store
	Derivatives *derivstore.Store
	Images      *derive.Converter
	Video       *video.Tool
	Exif        *exifdata.Reader
	// Manifest is the append-only recovery log, and this worker writes to it
	// for exactly one reason: joining a Snapchat recording back together
	// produces an original, and an original with no manifest line is a blob a
	// rebuild would not know what to do with. Optional — without it the merge
	// job refuses rather than archiving something unrecoverable, and every
	// other kind of work is unaffected.
	Manifest *manifest.Log
	Log      *slog.Logger
}

type Runner struct {
	Deps

	// MetadataWorkers handle exiftool and thumbnails: fast, and the gallery is
	// blocked on them, so this is the pool that gets the parallelism.
	MetadataWorkers int
	// TranscodeWorkers handle video. One by default: ffmpeg already saturates
	// several cores per clip, so a second worker mostly just competes with the
	// first.
	TranscodeWorkers int
	// SignatureWorkers reduce originals to the numbers the duplicate scan
	// compares.
	//
	// A third pool for the reason there is a second: this work decodes every
	// original in the archive and samples twenty frames out of every video, it
	// takes an hour over a library this size, and nobody is waiting for any of
	// it. Sharing the metadata pool would put the gallery's thumbnails behind
	// it; sharing the transcode pool would put the viewer's playback renditions
	// behind it. One by default, so a backfill is something the machine does in
	// the background rather than something it does instead of everything else.
	SignatureWorkers int

	// PollInterval is the floor on how often an idle worker looks for work.
	// Uploads nudge the pools directly, so this only matters for work that
	// appeared without one — a retry coming off backoff, or a reclaimed job.
	PollInterval time.Duration
	// SweepInterval is how often abandoned jobs are returned to the queue.
	SweepInterval time.Duration
	// HeartbeatInterval is how often a running job pushes its lease forward.
	HeartbeatInterval time.Duration

	mu     sync.Mutex
	nudges []chan struct{}
	wg     sync.WaitGroup
}

func New(deps Deps) *Runner {
	return &Runner{
		Deps:              deps,
		MetadataWorkers:   4,
		TranscodeWorkers:  1,
		SignatureWorkers:  1,
		PollInterval:      5 * time.Second,
		SweepInterval:     time.Minute,
		HeartbeatInterval: jobs.DefaultLease / 3,
	}
}

// Start launches the pools and returns immediately. Cancelling ctx stops them;
// Wait blocks until they have.
func (r *Runner) Start(ctx context.Context) {
	if n, err := jobs.ReconcileMetadata(ctx, r.Store.Pool()); err != nil {
		r.log().Error("reconcile metadata jobs", "error", err)
	} else if n > 0 {
		r.log().Info("queued derivative work for assets that had none", "assets", n)
	}
	if err := r.reconcileSignatures(ctx); err != nil {
		r.log().Error("reconcile signature jobs", "error", err)
	}

	for i := range r.metadataWorkers() {
		r.spawn(ctx, fmt.Sprintf("metadata-%d", i), []jobs.Kind{jobs.KindMetadata})
	}
	// The transcode pool takes the merges as well. Both are ffmpeg over a whole
	// video, both are minutes rather than milliseconds, and a merge that had its
	// own pool would be a fourth set of goroutines competing for the same cores.
	for i := range r.transcodeWorkers() {
		r.spawn(ctx, fmt.Sprintf("transcode-%d", i), []jobs.Kind{jobs.KindPlayback, jobs.KindMerge})
	}
	for i := range r.signatureWorkers() {
		r.spawn(ctx, fmt.Sprintf("signature-%d", i), []jobs.Kind{jobs.KindSignature})
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.sweep(ctx)
	}()
}

func (r *Runner) Wait() { r.wg.Wait() }

// Nudge wakes idle workers. The upload path calls it after committing an asset
// so the first thumbnail appears in about the time it takes to make one, rather
// than at the next poll.
func (r *Runner) Nudge() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.nudges {
		select {
		case ch <- struct{}{}:
		default: // already pending; one wake-up is enough
		}
	}
}

func (r *Runner) spawn(ctx context.Context, id string, kinds []jobs.Kind) {
	nudge := make(chan struct{}, 1)
	r.mu.Lock()
	r.nudges = append(r.nudges, nudge)
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.loop(ctx, id, kinds, nudge)
	}()
}

func (r *Runner) loop(ctx context.Context, id string, kinds []jobs.Kind, nudge <-chan struct{}) {
	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()

	for {
		job, err := r.Queue.Claim(ctx, kinds, id)
		switch {
		case errors.Is(err, jobs.ErrNoJob):
			// Idle: wait to be told, or look again on the next tick.
			select {
			case <-ctx.Done():
				return
			case <-nudge:
			case <-ticker.C:
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			r.log().Error("claim job", "worker", id, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}

		r.execute(ctx, id, job)
		if ctx.Err() != nil {
			return
		}
		// Straight back to claiming: a backlog should drain without waiting on
		// a tick between every item.
	}
}

// execute runs one job to a conclusion and records it. A job that panics is
// recorded as an ordinary failure, so one malformed file cannot take a pool
// down with it.
func (r *Runner) execute(ctx context.Context, workerID string, job jobs.Job) {
	stop := r.beat(ctx, job.ID)
	defer stop()

	started := time.Now()
	err := r.run(ctx, job)

	// A cancelled context is a shutdown, not a failure. Leaving the job running
	// lets the lease sweep return it to the queue on the next start; marking it
	// failed would spend an attempt on an interruption the job had no part in.
	if ctx.Err() != nil {
		return
	}

	if err != nil {
		permanent, failErr := r.Queue.Fail(context.WithoutCancel(ctx), job, err)
		if failErr != nil {
			r.log().Error("record job failure", "job", job.ID, "error", failErr)
		}
		r.log().Warn("derivative job failed",
			"job", job.ID, "kind", job.Kind, "asset", job.AssetID,
			"attempt", job.Attempts, "permanent", permanent, "error", err)
		if permanent {
			r.markFailed(context.WithoutCancel(ctx), job)
		}
		return
	}

	if err := r.Queue.Complete(ctx, job.ID); err != nil {
		r.log().Error("complete job", "job", job.ID, "error", err)
		return
	}
	r.log().Debug("derivative job done",
		"job", job.ID, "kind", job.Kind, "asset", job.AssetID, "took", time.Since(started))
}

// vaulted reports that this asset was hidden or archived while its job was in
// flight, which is the one way a job can find itself holding a row whose
// original is deliberately not on disk.
//
// Hiding deletes the queued work for an asset, but it cannot delete a job that
// is already running — so a metadata job that started a moment before is still
// going to look for a plaintext original that has just been encrypted away.
// Without this it fails, retries four more times, and marks a hidden photograph
// permanently broken.
//
// Done rather than failed, and silently. There is genuinely nothing left to do:
// the renditions were sealed on the way in, and if any were missing the restore
// requeues this exact job on the way out.
func vaulted(asset db.Asset) bool { return asset.Vault != "" }

func (r *Runner) run(ctx context.Context, job jobs.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in %s job: %v\n%s", job.Kind, p, debug.Stack())
		}
	}()

	switch job.Kind {
	case jobs.KindMetadata:
		return r.runMetadata(ctx, job.AssetID)
	case jobs.KindPlayback:
		return r.runPlayback(ctx, job.AssetID)
	case jobs.KindSignature:
		return r.runSignature(ctx, job.AssetID)
	case jobs.KindMerge:
		if r.Manifest == nil {
			return errors.New("no manifest log configured; refusing to archive a joined recording that a rebuild could not recover")
		}
		return r.runMerge(ctx, job.AssetID)
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// runMetadata reads an original and produces everything the gallery needs to
// draw it: the metadata row and a thumbnail at every stored size.
func (r *Runner) runMetadata(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
	}
	if vaulted(asset) {
		return nil
	}
	src := r.Blobs.Path(asset.SHA256, asset.Ext)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("original missing from the blob store: %w", err)
	}

	data, err := r.Exif.Read(ctx, src)
	if err != nil {
		return err
	}
	meta := metadataFrom(data)

	// Pairing, before anything is built, because it decides what to build. An
	// imported Live Photo declares nothing on upload — the export it came from
	// had nothing to declare with — so this read is the first moment anything
	// knows the file is half of one, and the branch below is the first
	// consequence.
	if data.ContentID != asset.ContentID {
		if asset, err = r.applyContentID(ctx, assetID, data.ContentID); err != nil {
			return err
		}
	}

	// The caption layer, for a Snapchat memory. Read before anything is
	// rendered, because every rendition below is built from the composite
	// rather than from the file this job was handed.
	overlay, err := r.overlay(ctx, asset)
	if err != nil {
		return err
	}

	decodable := true
	var thumbErr error
	switch {
	case asset.IsLivePair():
		if decodable, err = r.liveMetadata(ctx, asset, src, &meta); err != nil {
			return err
		}
	case asset.MediaKind == db.MediaVideo:
		if decodable, err = r.videoMetadata(ctx, asset, src, overlay, &meta); err != nil {
			return err
		}
	default:
		if overlay != nil {
			// What the file says, not what the row says: the row's width and
			// height are written by this job, and on a first run they are still
			// null. Zero leaves derive to measure the file itself, which is
			// correct and merely slower.
			overlay.Width, overlay.Height = intOr(meta.Width), intOr(meta.Height)
		}
		// Held rather than returned, so the metadata below is stored first. What
		// exiftool read is good whether or not the render worked, and a job that
		// parks having written nothing leaves an asset that looks unreadable
		// when only its thumbnail was — which is exactly how three JPEGs named
		// .dng came to look like corrupt files.
		thumbErr = r.writeThumbs(ctx, asset.SHA256, src, overlay)
	}

	if err := r.Store.ApplyMetadata(ctx, assetID, meta); err != nil {
		return err
	}
	if thumbErr != nil {
		return thumbErr
	}

	// The signature, queued here rather than beside this job, because a video
	// cannot be sampled without knowing how long it is and that number was
	// written a few lines above. Not for the components — a paired video and a
	// caption layer are parts of another asset's picture, and the duplicate
	// scan never looks at either.
	if !asset.IsLivePair() && !asset.IsOverlay {
		if err := jobs.Enqueue(ctx, r.Store.Pool(), jobs.KindSignature, assetID); err != nil {
			return err
		}
	}

	if asset.IsLivePair() {
		// No transcode is queued for these. The grid plays whichever of the
		// renditions just built matches its zoom level, and the viewer renders
		// its larger one per request — so a library where a third of the items
		// are Live Photos does not put a third of itself through the transcode
		// queue for three seconds each.
		state := db.DerivedReady
		if !decodable {
			state = db.DerivedFailed
		}
		if err := r.Store.SetLiveState(ctx, assetID, state); err != nil {
			return err
		}
		// An imported video can reach here on its second run, having been an
		// ordinary video on its first: it was archived before the still that
		// claims it, so nothing knew it was a pair until that still arrived.
		// Its playback rendition is now something no page will ever request.
		return r.Store.SetPlaybackState(ctx, assetID, db.PlaybackNone)
	}

	if asset.MediaKind == db.MediaVideo {
		// A container ffprobe can describe but ffmpeg cannot decode will not
		// transcode either, so queueing the work only buys five failing attempts
		// and a parked job. The metadata that was readable is already stored.
		if !decodable {
			return r.Store.SetPlaybackState(ctx, assetID, db.PlaybackNone)
		}
		if err := r.Store.SetPlaybackState(ctx, assetID, db.DerivedPending); err != nil {
			return err
		}
		if err := jobs.Enqueue(ctx, r.Store.Pool(), jobs.KindPlayback, assetID); err != nil {
			return err
		}
		r.Nudge()
	}
	return nil
}

// applyContentID records the Apple content identifier this file actually
// carries and returns the asset as pairing left it.
//
// The file overrules whatever a client declared, which is what makes an import
// pair at all: the declaration is a hint an importer offers to save a second
// pass, and the maker note is the fact. Resolving can also adopt videos that
// were already archived as ordinary ones — this still is the half that was
// missing — and those come back needing their motion renditions built.
func (r *Runner) applyContentID(ctx context.Context, assetID, contentID string) (db.Asset, error) {
	asset, requeued, err := r.Store.SetContentID(ctx, assetID, contentID)
	if err != nil {
		return db.Asset{}, err
	}
	if len(requeued) > 0 {
		r.log().Info("paired archived videos to a still that arrived after them",
			"still", assetID, "content_id", contentID, "videos", len(requeued))
		r.Nudge()
	}
	return asset, nil
}

// overlay resolves the caption layer an asset carries, or nil for the vast
// majority that carry none.
//
// The layer is an ordinary archived asset with its own blob, so this is a row
// read and a path — no second store, no special case in verify or reindex. A
// link pointing at a row that is gone is treated as no layer rather than as a
// failure: `on delete set null` means that cannot happen through the database,
// and a memory that renders without its caption beats one that never renders.
func (r *Runner) overlay(ctx context.Context, asset db.Asset) (*derive.Layer, error) {
	if asset.OverlayAssetID == nil {
		return nil, nil
	}
	layer, err := r.Store.Asset(ctx, *asset.OverlayAssetID)
	if errors.Is(err, db.ErrNotFound) {
		r.log().Warn("overlay is linked but missing; rendering without it",
			"asset", asset.ID, "overlay", *asset.OverlayAssetID)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	path := r.Blobs.Path(layer.SHA256, layer.Ext)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("overlay missing from the blob store: %w", err)
	}
	return &derive.Layer{Path: path}, nil
}

// intOr reads an optional dimension, and 0 stands for "not known" — which is
// exactly what derive.Layer's zero size already means.
func intOr(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// videoMetadata fills in what ffprobe knows better than exiftool and builds the
// poster thumbnail.
//
// The poster goes through a temp file and then the same thumbnailer the photos
// use, rather than piping ffmpeg into ImageMagick. Two subprocesses joined by a
// pipe fail in ways that are miserable to diagnose, and one square-thumbnail
// path means video tiles and photo tiles cannot drift apart.
// It reports whether the video decoded. A false with no error is the degraded
// case: ffprobe described the file, ffmpeg could not get a frame out of it, and
// what was readable has still been recorded.
func (r *Runner) videoMetadata(ctx context.Context, asset db.Asset, src string, overlay *derive.Layer, meta *db.Metadata) (bool, error) {
	info, err := r.Video.Probe(ctx, src)
	if err != nil {
		return false, err
	}

	if w, h := info.DisplaySize(); w > 0 && h > 0 {
		meta.Width, meta.Height = &w, &h
	}
	if info.DurationSeconds > 0 {
		d := info.DurationSeconds
		meta.DurationSeconds = &d
	}

	// The .jpg matters: ffmpeg picks its output format from the extension.
	poster, cleanup, err := r.Derivatives.Stage("poster-*.jpg")
	if err != nil {
		return false, err
	}
	defer cleanup()

	if err := r.Video.PosterFrame(ctx, src, poster, info); err != nil {
		// Losing a tile to a container ffmpeg cannot decode is a smaller loss
		// than parking the asset: the original is archived and verified either
		// way, and the gallery draws a placeholder for a thumbnail that 404s.
		if errors.Is(err, video.ErrNoFrame) {
			r.log().Warn("no poster frame; keeping what the probe read",
				"asset", asset.ID, "error", err)
			return false, nil
		}
		return false, err
	}
	// The poster comes out at the video's display size, so the layer is
	// stretched to that and the tile shows the memory the way it was sent —
	// caption and all — before anyone opens it.
	if overlay != nil {
		overlay.Width, overlay.Height = info.DisplaySize()
	}
	return true, r.writeThumbs(ctx, asset.SHA256, poster, overlay)
}

// liveMetadata handles a Live Photo's paired video: what ffprobe knows about
// it, and the motion thumbnails the grid plays on hover, one per stored size.
//
// No poster frame and no still thumbnail, unlike an ordinary video. The tile
// this asset appears in belongs to the still it is paired with, and that still
// has its own thumbnail from its own original — a poster extracted here would
// be a near-duplicate of it that nothing would ever draw.
//
// Like videoMetadata, it reports whether the file decoded rather than failing
// on a container ffprobe can describe and ffmpeg cannot open. What was readable
// is still worth recording; the cost is a photo that does not come to life.
func (r *Runner) liveMetadata(ctx context.Context, asset db.Asset, src string, meta *db.Metadata) (bool, error) {
	info, err := r.Video.Probe(ctx, src)
	if err != nil {
		return false, err
	}
	if w, h := info.DisplaySize(); w > 0 && h > 0 {
		meta.Width, meta.Height = &w, &h
	}
	if info.DurationSeconds > 0 {
		d := info.DurationSeconds
		meta.DurationSeconds = &d
	}

	targets := make([]video.LiveThumbTarget, 0, len(derivstore.ThumbSizes))
	for _, size := range derivstore.ThumbSizes {
		staged, cleanup, err := r.Derivatives.Stage("live-*" + derivstore.LiveSuffix(size))
		if err != nil {
			return false, err
		}
		defer cleanup()
		targets = append(targets, video.LiveThumbTarget{Size: size, Path: staged})
	}

	if err := r.Video.LiveThumbs(ctx, src, targets, info); err != nil {
		if errors.Is(err, video.ErrNotPlayable) {
			r.log().Warn("no motion thumbnail; the still will not come to life",
				"asset", asset.ID, "error", err)
			return false, nil
		}
		return false, err
	}
	for _, target := range targets {
		if err := r.Derivatives.Commit(asset.SHA256, derivstore.LiveSuffix(target.Size), target.Path); err != nil {
			return false, err
		}
	}
	return true, nil
}

// writeThumbs builds every stored size of a still and publishes them together.
//
// Together because the sizes are one rendition of one photo as far as anything
// downstream is concerned: a metadata job that wrote two of three and then died
// would leave an asset marked ready whose gallery goes blank at one zoom level
// and not the others. Staging them all and committing at the end means a failed
// run leaves exactly what was there before.
func (r *Runner) writeThumbs(ctx context.Context, sha, src string, overlay *derive.Layer) error {
	targets := make([]derive.ThumbTarget, 0, len(derivstore.ThumbSizes))
	for _, size := range derivstore.ThumbSizes {
		staged, cleanup, err := r.Derivatives.Stage("thumb-*.webp")
		if err != nil {
			return err
		}
		defer cleanup()
		targets = append(targets, derive.ThumbTarget{Size: size, Path: staged})
	}

	if err := r.Images.Thumbs(ctx, src, overlay, targets); err != nil {
		return err
	}
	for _, target := range targets {
		if err := r.Derivatives.Commit(sha, derivstore.ThumbSuffix(target.Size), target.Path); err != nil {
			return err
		}
	}
	return nil
}

// runPlayback builds the browser-playable rendition of a video.
func (r *Runner) runPlayback(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
	}
	if vaulted(asset) {
		return nil
	}
	if asset.MediaKind != db.MediaVideo {
		// Nothing to transcode. Not an error — reaching here means someone
		// queued the wrong kind, and failing would retry it four more times.
		return r.Store.SetPlaybackState(ctx, assetID, db.PlaybackNone)
	}
	if asset.IsLivePair() {
		// It was an ordinary video when this job was queued and it is a Live
		// Photo's motion now, because the still it belongs to was archived in
		// between. Nothing will ever open a player on it, so spending a
		// transcode here would be spending it on a rendition with no reader.
		return r.Store.SetPlaybackState(ctx, assetID, db.PlaybackNone)
	}

	src := r.Blobs.Path(asset.SHA256, asset.Ext)
	info, err := r.Video.Probe(ctx, src)
	if err != nil {
		return err
	}

	overlay, err := r.overlay(ctx, asset)
	if err != nil {
		return err
	}

	// The rendition every player gets. For a memory that is the composite, so
	// the caption is there for anything that just asks for the video — the
	// phone app, a saved link, the grid's own poster.
	burned := ""
	if overlay != nil {
		burned = overlay.Path
	}
	if err := r.transcodeTo(ctx, asset, src, info, burned, derivstore.Playback); err != nil {
		return err
	}

	// And the photograph underneath, for the viewer's toggle. Second rather than
	// instead: this one is the extra, and a failure to build it should cost the
	// toggle rather than the video. It is written before the state flips to
	// ready so the two are never briefly out of step.
	if overlay != nil {
		if err := r.transcodeTo(ctx, asset, src, info, "", derivstore.PlaybackPlain); err != nil {
			return err
		}
	}
	return r.Store.SetPlaybackState(ctx, assetID, db.DerivedReady)
}

// transcodeTo builds one playback rendition and publishes it under the given
// suffix.
func (r *Runner) transcodeTo(ctx context.Context, asset db.Asset, src string, info video.Info, overlay, suffix string) error {
	// Same extension requirement as the poster: ffmpeg reads the container off
	// the name, and +faststart needs a real seekable file to rewrite at the end.
	staged, cleanup, err := r.Derivatives.Stage("transcode-*" + derivstore.Playback)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := r.Video.Transcode(ctx, src, staged, info, overlay); err != nil {
		return err
	}
	return r.Derivatives.Commit(asset.SHA256, suffix, staged)
}

// markFailed records a permanently failed job on the asset, which is what turns
// a tile that spins forever into one that says something went wrong.
func (r *Runner) markFailed(ctx context.Context, job jobs.Job) {
	var err error
	switch job.Kind {
	case jobs.KindMetadata:
		err = r.Store.SetDerivedState(ctx, job.AssetID, db.DerivedFailed)
		// The motion thumbnail is built by this job too, so it went down with
		// it. Saying so is what stops the grid waiting on a rendition that is
		// never coming; the setter ignores assets that have no motion.
		if liveErr := r.Store.SetLiveState(ctx, job.AssetID, db.DerivedFailed); liveErr != nil && err == nil {
			err = liveErr
		}
	case jobs.KindPlayback:
		err = r.Store.SetPlaybackState(ctx, job.AssetID, db.DerivedFailed)
	}
	if err != nil {
		r.log().Error("mark asset failed", "asset", job.AssetID, "error", err)
	}
}

// beat keeps a running job's lease alive and returns a stop function.
func (r *Runner) beat(ctx context.Context, id int64) func() {
	beatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(r.heartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
				if err := r.Queue.Heartbeat(beatCtx, id); err != nil && beatCtx.Err() == nil {
					r.log().Warn("heartbeat job", "job", id, "error", err)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func (r *Runner) sweep(ctx context.Context) {
	ticker := time.NewTicker(r.sweepInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.Queue.ReclaimExpired(ctx)
			if err != nil {
				if ctx.Err() == nil {
					r.log().Error("reclaim expired jobs", "error", err)
				}
				continue
			}
			if n > 0 {
				r.log().Info("returned abandoned jobs to the queue", "jobs", n)
				r.Nudge()
			}
		}
	}
}

// metadataFrom maps what the file said onto what the database stores.
func metadataFrom(d exifdata.Data) db.Metadata {
	width, height := d.DisplaySize()
	return db.Metadata{
		Width:             width,
		Height:            height,
		Orientation:       d.Orientation,
		DurationSeconds:   d.DurationSeconds,
		CameraMake:        d.CameraMake,
		CameraModel:       d.CameraModel,
		Lens:              d.Lens,
		GPSLat:            d.GPSLat,
		GPSLon:            d.GPSLon,
		ExifCapturedAt:    d.CapturedAt,
		ExifOffsetMinutes: d.OffsetMinutes,

		Raw:          d.Raw,
		GPSAltitude:  d.GPSAltitude,
		GPSDirection: d.GPSDirection,
		GPSAccuracy:  d.GPSAccuracy,
		GPSAt:        d.GPSAt,

		ISO:             d.ISO,
		FNumber:         d.FNumber,
		ExposureSeconds: d.ExposureSeconds,
		FocalLength:     d.FocalLength,
		FocalLength35:   d.FocalLength35,
		Flash:           d.Flash,

		Description:  d.Description,
		ColorProfile: d.ColorProfile,
		CaptureType:  d.CaptureType,

		VideoCodec:    d.VideoCodec,
		FrameRate:     d.FrameRate,
		Bitrate:       d.Bitrate,
		AudioCodec:    d.AudioCodec,
		AudioChannels: d.AudioChannels,

		Faces: encodeFaces(d.Faces),
	}
}

// encodeFaces renders the region list for the jsonb column, and nothing at all
// when there were no regions — an empty array would read as "something looked
// and found no faces", which is not what an absent XMP region list means.
func encodeFaces(faces []exifdata.Face) json.RawMessage {
	if len(faces) == 0 {
		return nil
	}
	encoded, err := json.Marshal(faces)
	if err != nil {
		return nil
	}
	return encoded
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Runner) metadataWorkers() int {
	if r.MetadataWorkers <= 0 {
		return 1
	}
	return r.MetadataWorkers
}

func (r *Runner) transcodeWorkers() int {
	if r.TranscodeWorkers <= 0 {
		return 1
	}
	return r.TranscodeWorkers
}

func (r *Runner) signatureWorkers() int {
	if r.SignatureWorkers <= 0 {
		return 1
	}
	return r.SignatureWorkers
}

func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval <= 0 {
		return 5 * time.Second
	}
	return r.PollInterval
}

func (r *Runner) sweepInterval() time.Duration {
	if r.SweepInterval <= 0 {
		return time.Minute
	}
	return r.SweepInterval
}

func (r *Runner) heartbeatInterval() time.Duration {
	if r.HeartbeatInterval <= 0 {
		return jobs.DefaultLease / 3
	}
	return r.HeartbeatInterval
}
