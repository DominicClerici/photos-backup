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
	"errors"
	"fmt"
	"io"
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
	Log         *slog.Logger
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

	for i := range r.metadataWorkers() {
		r.spawn(ctx, fmt.Sprintf("metadata-%d", i), []jobs.Kind{jobs.KindMetadata})
	}
	for i := range r.transcodeWorkers() {
		r.spawn(ctx, fmt.Sprintf("transcode-%d", i), []jobs.Kind{jobs.KindPlayback})
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
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// runMetadata reads an original and produces everything the gallery needs to
// draw it: the metadata row and the 256px thumbnail.
func (r *Runner) runMetadata(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
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

	decodable := true
	if asset.MediaKind == db.MediaVideo {
		if decodable, err = r.videoMetadata(ctx, asset, src, &meta); err != nil {
			return err
		}
	} else if err := r.writeThumb(ctx, asset.SHA256, src); err != nil {
		return err
	}

	if err := r.Store.ApplyMetadata(ctx, assetID, meta); err != nil {
		return err
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
func (r *Runner) videoMetadata(ctx context.Context, asset db.Asset, src string, meta *db.Metadata) (bool, error) {
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
	return true, r.writeThumb(ctx, asset.SHA256, poster)
}

func (r *Runner) writeThumb(ctx context.Context, sha, src string) error {
	return r.Derivatives.Write(sha, derivstore.Thumb, func(w io.Writer) error {
		return r.Images.Thumb(ctx, src, w)
	})
}

// runPlayback builds the browser-playable rendition of a video.
func (r *Runner) runPlayback(ctx context.Context, assetID string) error {
	asset, err := r.Store.Asset(ctx, assetID)
	if err != nil {
		return err
	}
	if asset.MediaKind != db.MediaVideo {
		// Nothing to transcode. Not an error — reaching here means someone
		// queued the wrong kind, and failing would retry it four more times.
		return r.Store.SetPlaybackState(ctx, assetID, db.PlaybackNone)
	}

	src := r.Blobs.Path(asset.SHA256, asset.Ext)
	info, err := r.Video.Probe(ctx, src)
	if err != nil {
		return err
	}

	// Same extension requirement as the poster: ffmpeg reads the container off
	// the name, and +faststart needs a real seekable file to rewrite at the end.
	staged, cleanup, err := r.Derivatives.Stage("transcode-*" + derivstore.Playback)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := r.Video.Transcode(ctx, src, staged, info); err != nil {
		return err
	}
	if err := r.Derivatives.Commit(asset.SHA256, derivstore.Playback, staged); err != nil {
		return err
	}
	return r.Store.SetPlaybackState(ctx, assetID, db.DerivedReady)
}

// markFailed records a permanently failed job on the asset, which is what turns
// a tile that spins forever into one that says something went wrong.
func (r *Runner) markFailed(ctx context.Context, job jobs.Job) {
	var err error
	switch job.Kind {
	case jobs.KindMetadata:
		err = r.Store.SetDerivedState(ctx, job.AssetID, db.DerivedFailed)
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
	}
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
