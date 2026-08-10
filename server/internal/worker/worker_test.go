package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

func TestMetadataJobThumbnailsAPhotoAndReadsItsEXIF(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, asset.ID)
	if got.DerivedState != db.DerivedReady {
		t.Fatalf("DerivedState = %q, want ready", got.DerivedState)
	}

	// Portrait: the worker must store what you see, not the sensor's landscape read.
	if got.Width == nil || *got.Width != 3024 || got.Height == nil || *got.Height != 4032 {
		t.Errorf("size = %v x %v, want 3024x4032", got.Width, got.Height)
	}
	if got.CameraModel != "iPhone 14 Pro" {
		t.Errorf("CameraModel = %q", got.CameraModel)
	}
	if got.ExifCapturedAt == nil {
		t.Error("ExifCapturedAt is nil for a photo that carries DateTimeOriginal")
	}
	if got.GPSLat == nil || got.GPSLon == nil {
		t.Error("GPS was not recorded")
	}

	stat, err := os.Stat(h.Derivatives.Path(asset.SHA256, derivstore.Thumb))
	if err != nil {
		t.Fatalf("thumbnail was not written: %v", err)
	}
	if stat.Size() == 0 {
		t.Error("thumbnail is empty")
	}

	// A photo has nothing to transcode, so nothing must be queued for it.
	if got.PlaybackState != db.PlaybackNone {
		t.Errorf("PlaybackState = %q, want none for a photo", got.PlaybackState)
	}
	if _, err := h.Queue.Claim(context.Background(), []jobs.Kind{jobs.KindPlayback}, "t"); err == nil {
		t.Error("a photo queued a playback job")
	}
}

// A file with no metadata at all still has to come out with a thumbnail and a
// place on the timeline. Screenshots and messaging-app images arrive this way.
func TestMetadataJobHandlesAFileWithNoEXIF(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, asset.ID)
	if got.DerivedState != db.DerivedReady {
		t.Fatalf("DerivedState = %q, want ready", got.DerivedState)
	}
	if got.ExifCapturedAt != nil {
		t.Errorf("ExifCapturedAt = %v, want nil", got.ExifCapturedAt)
	}
	if !h.Derivatives.Exists(asset.SHA256, derivstore.Thumb) {
		t.Error("no thumbnail for a file without EXIF")
	}
	// With no capture time in the file, the timeline falls back to the phone's,
	// then to arrival. It must never be zero.
	if got.SortTime.IsZero() {
		t.Error("SortTime is zero")
	}
}

func TestMetadataJobPostersAVideoAndQueuesTheTranscode(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, asset.ID)
	if got.DerivedState != db.DerivedReady {
		t.Fatalf("DerivedState = %q, want ready", got.DerivedState)
	}
	if got.DurationSeconds == nil || *got.DurationSeconds < 0.5 {
		t.Errorf("DurationSeconds = %v, want ~1", got.DurationSeconds)
	}
	if got.Width == nil || *got.Width != 640 {
		t.Errorf("Width = %v, want 640", got.Width)
	}
	// The poster goes through the same square thumbnailer photos use, so video
	// tiles and photo tiles cannot drift apart.
	if !h.Derivatives.Exists(asset.SHA256, derivstore.Thumb) {
		t.Fatal("no poster thumbnail for the video")
	}

	if got.PlaybackState != db.DerivedPending {
		t.Errorf("PlaybackState = %q, want pending", got.PlaybackState)
	}
	job, err := h.Queue.Claim(context.Background(), []jobs.Kind{jobs.KindPlayback}, "t")
	if err != nil {
		t.Fatalf("the transcode was not queued: %v", err)
	}
	if job.AssetID != asset.ID {
		t.Errorf("queued playback for %s, want %s", job.AssetID, asset.ID)
	}
}

func TestPlaybackJobProducesAnMP4(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindPlayback)

	got := h.reload(t, asset.ID)
	if got.PlaybackState != db.DerivedReady {
		t.Fatalf("PlaybackState = %q, want ready", got.PlaybackState)
	}

	stat, err := os.Stat(h.Derivatives.Path(asset.SHA256, derivstore.Playback))
	if err != nil {
		t.Fatalf("playback file was not written: %v", err)
	}
	if stat.Size() == 0 {
		t.Error("playback file is empty")
	}

	info, err := h.Video.Probe(context.Background(), h.Derivatives.Path(asset.SHA256, derivstore.Playback))
	if err != nil {
		t.Fatalf("probe the playback file: %v", err)
	}
	if info.Width != 640 || info.Height != 480 {
		t.Errorf("playback is %dx%d, want 640x480", info.Width, info.Height)
	}
}

// Queuing a transcode for a photo is a bug somewhere else, but retrying it five
// times and then flagging the asset as broken would turn that bug into a
// visible error on a perfectly good photo.
func TestPlaybackJobOnAPhotoSettlesInsteadOfFailing(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	if err := jobs.Enqueue(context.Background(), h.store.Pool(), jobs.KindPlayback, asset.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	h.claimAndRun(t, jobs.KindPlayback)

	if state := h.jobState(t, asset.ID, jobs.KindPlayback); state != string(jobs.StateDone) {
		t.Errorf("job state = %q, want done", state)
	}
	if got := h.reload(t, asset.ID); got.PlaybackState != db.PlaybackNone {
		t.Errorf("PlaybackState = %q, want none", got.PlaybackState)
	}
}

func TestJobRetriesThenMarksTheAssetFailed(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	// Delete the original out from under the job. This is the shape of every
	// permanent failure — a file the tools cannot turn into a thumbnail.
	if err := os.Remove(h.Blobs.Path(asset.SHA256, asset.Ext)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	h.Queue.MaxAttempts = 2
	h.Queue.BaseBackoff = time.Nanosecond
	h.Queue.MaxBackoff = time.Nanosecond

	for range h.Queue.MaxAttempts {
		h.claimAndRun(t, jobs.KindMetadata)
	}

	if state := h.jobState(t, asset.ID, jobs.KindMetadata); state != string(jobs.StateFailed) {
		t.Fatalf("job state = %q, want failed", state)
	}
	if got := h.reload(t, asset.ID); got.DerivedState != db.DerivedFailed {
		t.Errorf("DerivedState = %q, want failed", got.DerivedState)
	}

	// The reason has to survive to the failure list, or there is no way to find
	// out what went wrong.
	failed, err := h.Queue.Failed(context.Background(), 10)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(failed) != 1 || failed[0].Error == "" {
		t.Fatalf("failed jobs = %+v, want one with an error message", failed)
	}
}

// A transient failure must not leave a permanent mark. The asset stays pending
// and the queue tries again.
func TestATransientFailureLeavesTheAssetPending(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	blob := h.Blobs.Path(asset.SHA256, asset.Ext)
	body, err := os.ReadFile(blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if err := os.Remove(blob); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	h.Queue.MaxAttempts = 5
	h.Queue.BaseBackoff = time.Nanosecond
	h.Queue.MaxBackoff = time.Nanosecond
	h.claimAndRun(t, jobs.KindMetadata)

	if got := h.reload(t, asset.ID); got.DerivedState != db.DerivedPending {
		t.Errorf("DerivedState = %q after one failure, want pending", got.DerivedState)
	}

	// Put it back; the retry has to succeed rather than stay poisoned.
	if err := os.WriteFile(blob, body, 0o644); err != nil {
		t.Fatalf("restore blob: %v", err)
	}
	h.claimAndRun(t, jobs.KindMetadata)

	if got := h.reload(t, asset.ID); got.DerivedState != db.DerivedReady {
		t.Errorf("DerivedState = %q after the retry, want ready", got.DerivedState)
	}
}

// A job that panics must be recorded as an ordinary failure. If the panic
// escaped, one malformed file would take a whole pool down.
func TestAPanicIsRecordedAsAFailure(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	// A nil Exif reader panics inside the handler.
	h.Exif = nil
	h.Queue.MaxAttempts = 1

	h.claimAndRun(t, jobs.KindMetadata)

	if state := h.jobState(t, asset.ID, jobs.KindMetadata); state != string(jobs.StateFailed) {
		t.Fatalf("job state = %q, want failed", state)
	}
	failed, err := h.Queue.Failed(context.Background(), 10)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed jobs = %d, want 1", len(failed))
	}
	if !strings.Contains(failed[0].Error, "panic") {
		t.Errorf("recorded error = %q, want it to name the panic", failed[0].Error)
	}
}

// The whole pipeline, driven by the pools rather than by hand: a photo and a
// video go in, and everything derivable comes out without further prompting.
func TestRunnerDrainsTheQueue(t *testing.T) {
	h := newHarness(t)
	photo := h.ingest(t, "iphone-portrait.heic", db.MediaImage)
	clip := h.ingest(t, "clip.mov", db.MediaVideo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.MetadataWorkers = 2
	h.TranscodeWorkers = 1
	h.PollInterval = 50 * time.Millisecond
	h.Start(ctx)

	h.waitFor(t, "both assets to be derived", func() bool {
		return h.reload(t, photo.ID).DerivedState == db.DerivedReady &&
			h.reload(t, clip.ID).DerivedState == db.DerivedReady
	})
	h.waitFor(t, "the video to finish transcoding", func() bool {
		return h.reload(t, clip.ID).PlaybackState == db.DerivedReady
	})

	if !h.Derivatives.Exists(photo.SHA256, derivstore.Thumb) {
		t.Error("no thumbnail for the photo")
	}
	if !h.Derivatives.Exists(clip.SHA256, derivstore.Playback) {
		t.Error("no playback file for the video")
	}

	cancel()
	h.Wait()
}

// Shutdown must not spend an attempt. The job stays running so the lease sweep
// returns it to the queue on the next start.
func TestShutdownDoesNotFailTheJobInFlight(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	ctx, cancel := context.WithCancel(context.Background())
	job, err := h.Queue.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "test-worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	cancel()
	h.execute(ctx, "test-worker", job)

	if state := h.jobState(t, asset.ID, jobs.KindMetadata); state != string(jobs.StateRunning) {
		t.Errorf("job state = %q after shutdown, want it left running for the sweep", state)
	}
	if got := h.reload(t, asset.ID); got.DerivedState != db.DerivedPending {
		t.Errorf("DerivedState = %q, want pending", got.DerivedState)
	}
}

func TestReconcileQueuesWorkForAnAssetWithNoJob(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "bare.jpg", db.MediaImage)

	if _, err := h.store.Pool().Exec(context.Background(), "delete from jobs"); err != nil {
		t.Fatalf("clear jobs: %v", err)
	}

	n, err := jobs.ReconcileMetadata(context.Background(), h.store.Pool())
	if err != nil {
		t.Fatalf("ReconcileMetadata: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d assets, want 1", n)
	}

	job, err := h.Queue.Claim(context.Background(), []jobs.Kind{jobs.KindMetadata}, "t")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job.AssetID != asset.ID {
		t.Errorf("queued %s, want %s", job.AssetID, asset.ID)
	}
}
