// Command photod serves the photo archive: uploads in, gallery out, and the
// derivative workers that turn one into the other.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/api"
	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/discovery"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/video"
	"github.com/dominicclerici/photos-backup/server/internal/worker"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("photod exited", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg := config.FromEnv()

	root, err := filepath.Abs(cfg.PhotosRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	derivRoot := cfg.DerivativesRoot
	if derivRoot == "" {
		derivRoot = filepath.Join(root, "derivatives")
	}
	if derivRoot, err = filepath.Abs(derivRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(derivRoot, 0o755); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(); err != nil {
		return err
	}

	converter := derive.New()
	converter.Binary = cfg.MagickBin
	converter.PreviewConcurrency = cfg.PreviewConcurrency

	queue := jobs.NewQueue(store.Pool())
	blobs := blobstore.New(root)
	derivatives := derivstore.New(derivRoot)
	// Beside the blobs, not on the SSD: a committed session becomes a blob by
	// rename, and a rename only works within one filesystem.
	staging := uploads.New(filepath.Join(root, "incoming"))

	srv := &api.Server{
		Store:       store,
		Blobs:       blobs,
		Derivatives: derivatives,
		Manifest:    manifest.New(filepath.Join(root, "manifest.jsonl")),
		Converter:   converter,
		Queue:       queue,
		Uploads:     staging,
		Log:         log,
	}

	go sweepUploads(ctx, log, staging, cfg.UploadSessionTTL)

	// Missing tooling is reported, never fatal. An upload has to be able to
	// reach the disk on a host where ffmpeg was never installed — the archive
	// is the point, and derivatives are a convenience built on top of it. The
	// same rule PROJECT.md applies to photo-ml, one layer down.
	checkTools(log, cfg)

	if !cfg.WorkerDisabled {
		videoTool := video.New()
		videoTool.FFmpeg = cfg.FFmpegBin
		videoTool.FFprobe = cfg.FFprobeBin
		videoTool.Encoder = cfg.VideoEncoder

		exif := exifdata.New()
		exif.Binary = cfg.ExiftoolBin

		workers := worker.New(worker.Deps{
			Store:       store,
			Queue:       queue,
			Blobs:       blobs,
			Derivatives: derivatives,
			Images:      converter,
			Video:       videoTool,
			Exif:        exif,
			Log:         log,
		})
		workers.MetadataWorkers = cfg.WorkerConcurrency
		workers.TranscodeWorkers = cfg.TranscodeConcurrency

		workers.Start(ctx)
		defer workers.Wait()

		// The upload handler wakes the pools directly, so the first thumbnail
		// of a fresh backup appears in about the time it takes to make one.
		srv.Nudge = workers.Nudge

		log.Info("derivative workers running",
			"metadata", workers.MetadataWorkers,
			"transcode", workers.TranscodeWorkers,
			"derivatives_root", derivRoot)
	} else {
		log.Warn("derivative workers disabled; uploads will queue work that nothing drains")
	}

	// mDNS is a convenience, not a dependency: a responder that cannot bind
	// alongside the system one leaves the phone to use its last known good
	// address or a manually entered one, so this logs and carries on.
	if !cfg.MDNSDisabled {
		port, err := discovery.PortFrom(cfg.ListenAddr)
		if err != nil {
			log.Warn("not advertising over mDNS", "error", err)
		} else if ad, err := discovery.Advertise(cfg.MDNSInstance, port); err != nil {
			log.Warn("not advertising over mDNS", "error", err, "hint", "set MDNS_DISABLED=1 and publish via Avahi instead")
		} else {
			defer ad.Shutdown()
			log.Info("advertising over mDNS", "service", discovery.ServiceType, "host", ad.Host, "port", port)
		}
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a large original on a slow phone connection is a
		// normal upload, not a stalled one.
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("photod listening", "addr", cfg.ListenAddr, "photos_root", root)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// sweepUploads clears abandoned partial uploads, once at startup and hourly
// after that.
//
// A phone that gives up on a video mid-transfer leaves its bytes behind, and
// nothing else ever references them. Without this the archive quietly grows a
// pile of partials that `du` can see and no query can explain.
func sweepUploads(ctx context.Context, log *slog.Logger, staging *uploads.Store, ttl time.Duration) {
	sweep := func() {
		removed, err := staging.Sweep(ttl)
		if err != nil {
			log.Warn("could not sweep partial uploads", "error", err, "dir", staging.Dir())
			return
		}
		if removed > 0 {
			log.Info("swept abandoned partial uploads", "count", removed, "older_than", ttl)
		}
	}

	sweep()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// checkTools reports which external tools are missing, and what stops working
// without each one. Finding out at startup beats finding out from a queue full
// of identical failures an hour into a backfill.
func checkTools(log *slog.Logger, cfg config.Config) {
	for _, tool := range []struct{ binary, needed string }{
		{cfg.MagickBin, "thumbnails and previews"},
		{cfg.ExiftoolBin, "capture times, camera, and GPS"},
		{cfg.FFmpegBin, "video posters and playback renditions"},
		{cfg.FFprobeBin, "video dimensions and duration"},
	} {
		if _, err := exec.LookPath(tool.binary); err != nil {
			log.Warn("external tool not found; uploads still work but derivatives will fail",
				"binary", tool.binary, "affects", tool.needed)
		}
	}
}
