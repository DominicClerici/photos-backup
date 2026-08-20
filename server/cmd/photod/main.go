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
	"strings"
	"syscall"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/api"
	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/config"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/discovery"
	"github.com/dominicclerici/photos-backup/server/internal/exifdata"
	"github.com/dominicclerici/photos-backup/server/internal/geocode"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/livecache"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
	"github.com/dominicclerici/photos-backup/server/internal/purge"
	"github.com/dominicclerici/photos-backup/server/internal/tlsca"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
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

	// Resolved but deliberately not created. It holds files somebody downloads,
	// and an empty directory photod made for itself would look like an install
	// that had happened.
	geoDir, err := filepath.Abs(cfg.GeoNamesDir)
	if err != nil {
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

	// Shared with the worker pools below rather than built twice. The pools
	// build the stored renditions and the API renders Live Photos on demand,
	// and both are the same ffmpeg configured the same way.
	videoTool := video.New()
	videoTool.FFmpeg = cfg.FFmpegBin
	videoTool.FFprobe = cfg.FFprobeBin
	videoTool.Encoder = cfg.VideoEncoder
	videoTool.LiveConcurrency = cfg.LivePreviewConcurrency

	queue := jobs.NewQueue(store.Pool())
	blobs := blobstore.New(root)
	derivatives := derivstore.New(derivRoot)
	// Beside the blobs, not on the SSD: a committed session becomes a blob by
	// rename, and a rename only works within one filesystem.
	staging := uploads.New(filepath.Join(root, "incoming"))
	paired := devices.New(store.Pool())

	// The encrypted trees mirror the plaintext ones, one per disk. Hiding a
	// photograph must not quietly move a 500MB video off the archive drive and
	// onto the SSD the derivatives live on, so the originals' vault sits beside
	// the blobs and the renditions' beside the renditions.
	vaults := &vault.Service{
		Store:            store,
		Blobs:            blobs,
		Derivatives:      derivatives,
		VaultBlobs:       vault.NewStore(filepath.Join(root, "vault")),
		VaultDerivatives: vault.NewStore(filepath.Join(derivRoot, "vault")),
		Manifest:         manifest.New(filepath.Join(root, "manifest.jsonl")),
		Keeper:           vault.NewKeeper(cfg.VaultIdle),
		Log:              log,
	}

	srv := &api.Server{
		Store:        store,
		Blobs:        blobs,
		Derivatives:  derivatives,
		Manifest:     manifest.New(filepath.Join(root, "manifest.jsonl")),
		Converter:    converter,
		Video:        videoTool,
		LivePreviews: livecache.New(cfg.LivePreviewCacheBytes),
		Queue:        queue,
		Uploads:      staging,
		Devices:      paired,
		Vault:        vaults,
		// What the status page needs to tell a degraded server from a busy one.
		WorkerEnabled: !cfg.WorkerDisabled,
		Tools:         mediaTools(cfg),
		Log:           log,
	}

	go sweepUploads(ctx, log, staging, cfg.UploadSessionTTL)

	if cfg.PurgeDisabled {
		log.Warn("the trash sweep is disabled; deleted items will wait indefinitely")
	} else {
		go sweepTrash(ctx, purge.Deps{
			Store:       store,
			Blobs:       blobs,
			Derivatives: derivatives,
			Manifest:    srv.Manifest,
			Log:         log,
		}, cfg.PurgeInterval)
	}

	// On the same timer, and for a reason the trash sweep does not have: a
	// crash between "this photograph is hidden" and "its plaintext is gone"
	// leaves the original readable on the archive drive. It is a small window
	// and it will probably never open, but "probably never" is not the standard
	// a vault is held to. See vault.Service.Reconcile.
	go sweepVault(ctx, vaults, cfg.PurgeInterval)

	// And on the same timer again, for the one file in the derivative tree that
	// no per-asset cleanup can reach. See sweepJoinPreviews.
	go sweepJoinPreviews(ctx, store, derivatives, log, cfg.PurgeInterval)

	// Before the worker pools start, because unlike everything below this can
	// fail: TLS is not optional the way the media tools are. A missing ffmpeg
	// costs thumbnails, while serving the upload path in the clear would put a
	// device token on the wire — so this stops the daemon instead of degrading,
	// and it does so while returning early is still free.
	var certs *tlsca.Manager
	if !cfg.TLSDisabled {
		tlsDir := cfg.TLSDir
		if tlsDir == "" {
			tlsDir = filepath.Join(root, "tls")
		}
		if certs, err = tlsca.Open(tlsDir, cfg.TLSExtraSANs, log); err != nil {
			return err
		}
		fingerprint, _ := certs.Fingerprints()
		log.Info("TLS ready",
			"ca", certs.CACertPath(),
			"ca_sha256", fingerprint,
			"certifies", strings.Join(certs.SANs(), ","),
			"expires", certs.NotAfter().Format(time.RFC3339))
		go certs.Watch(ctx.Done(), 5*time.Minute)
	} else {
		log.Warn("TLS_DISABLED is set: the upload path is served in the clear and device tokens will cross the network unencrypted — development only")
	}

	// Missing tooling is reported, never fatal. An upload has to be able to
	// reach the disk on a host where ffmpeg was never installed — the archive
	// is the point, and derivatives are a convenience built on top of it. The
	// same rule PROJECT.md applies to photo-ml, one layer down.
	checkTools(log, cfg)

	if !cfg.WorkerDisabled {
		exif := exifdata.New()
		exif.Binary = cfg.ExiftoolBin

		// The one dependency the archive is designed to be able to lose. A
		// missing photo-ml is reported the way a missing geocoder is — once, at
		// startup, saying what is lost — and everything else runs unchanged.
		// See PROJECT.md §4.
		var ml *mlclient.Client
		if cfg.MLURL != "" {
			ml = mlclient.New(cfg.MLURL)
			log.Info("photo-ml configured; photographs will be searchable by what they show",
				"url", cfg.MLURL, "model", db.VisionModel, "vision_workers", cfg.VisionConcurrency)
		} else {
			log.Info("no ML_URL; photographs keep their dates, places and filenames and will not be searchable by what they show",
				"hint", "photo-ml/README.md, then set ML_URL=http://127.0.0.1:8789")
		}

		workers := worker.New(worker.Deps{
			Store:       store,
			Queue:       queue,
			Blobs:       blobs,
			Derivatives: derivatives,
			Images:      converter,
			Video:       videoTool,
			Exif:        exif,
			Places:      geocode.NewLoader(geoDir),
			Manifest:    srv.Manifest,
			ML:          ml,
			Log:         log,
		})
		workers.MetadataWorkers = cfg.WorkerConcurrency
		workers.TranscodeWorkers = cfg.TranscodeConcurrency
		workers.SignatureWorkers = cfg.SignatureConcurrency
		workers.PrepWorkers = cfg.PrepConcurrency
		workers.VisionWorkers = cfg.VisionConcurrency

		workers.Start(ctx)
		defer workers.Wait()

		// The upload handler wakes the pools directly, so the first thumbnail
		// of a fresh backup appears in about the time it takes to make one.
		srv.Nudge = workers.Nudge

		// The review page and the overview card are read off tables this fills,
		// so the server comes up having already looked. It is also what puts a
		// freshly imported Snapchat export back together without anybody asking
		// — see worker.Scan for why this is on startup rather than on a timer.
		srv.Scan = workers.Scan
		go func() {
			if _, err := workers.Scan(ctx); err != nil && ctx.Err() == nil {
				log.Error("scan for duplicates and split recordings", "error", err)
			}
		}()

		log.Info("derivative workers running",
			"metadata", workers.MetadataWorkers,
			"transcode", workers.TranscodeWorkers,
			"signature", workers.SignatureWorkers,
			"prep", workers.PrepWorkers,
			"vision", visionPoolSize(ml, workers.VisionWorkers),
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

	// Buffered for every listener, so a goroutine reporting a failure after
	// another already has cannot block forever on an unread channel.
	errs := make(chan error, 2)
	var servers []*http.Server

	apiSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a large original on a slow phone connection is a
		// normal upload, not a stalled one.
	}
	servers = append(servers, apiSrv)
	go func() {
		if certs == nil {
			log.Info("photod listening (http, no TLS)", "addr", cfg.ListenAddr, "photos_root", root)
			serveErr(errs, apiSrv.ListenAndServe())
			return
		}
		apiSrv.TLSConfig = certs.TLSConfig()
		log.Info("photod listening (https)", "addr", cfg.ListenAddr, "photos_root", root)
		// Certificate and key are supplied by TLSConfig.GetCertificate, which is
		// what lets a reissued leaf take effect without a restart.
		serveErr(errs, apiSrv.ListenAndServeTLS("", ""))
	}()

	// The read-only plaintext listener. The gallery and the maintenance CLI live
	// on this machine and would otherwise have to trust a private CA to read a
	// thumbnail; the write path is not served here at all.
	if cfg.PlaintextAddr != "" && !cfg.TLSDisabled {
		plain := &http.Server{
			Addr:              cfg.PlaintextAddr,
			Handler:           srv.PlaintextHandler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, plain)
		go func() {
			log.Info("serving the gallery read path in the clear", "addr", cfg.PlaintextAddr,
				"note", "no pairing and no upload endpoints are reachable here")
			serveErr(errs, plain.ListenAndServe())
		}()
	}

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var shutdownErr error
		for _, server := range servers {
			if err := server.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
				shutdownErr = err
			}
		}
		return shutdownErr
	}
}

// visionPoolSize reports zero when there is no photo-ml, because that is what
// is actually running. A log line claiming one vision worker on a machine with
// no GPU service would be describing a pool that was never started.
func visionPoolSize(ml *mlclient.Client, configured int) int {
	if ml == nil {
		return 0
	}
	return configured
}

func serveErr(errs chan<- error, err error) {
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs <- err
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

// sweepTrash destroys what the trash has finished holding, once at startup and
// on a timer after that.
//
// It runs here rather than on a systemd timer beside `verify` for the same
// reason the upload sweep does: the expiry is a property of the archive, not of
// how somebody chose to deploy it, and a retention that only elapses on hosts
// where a second unit was installed is not a retention.
//
// One bite at a time. Everything due is found by one index probe, but a library
// that has been deleting for a year could have an unbounded amount of it, and
// unlinking a hundred thousand files inside one tick is the kind of thing that
// makes a server look hung.
func sweepTrash(ctx context.Context, d purge.Deps, every time.Duration) {
	const perSweep = 1000

	sweep := func() {
		result, err := purge.Expired(ctx, d, perSweep)
		if err != nil {
			// The rows either went or they did not; either way the next tick
			// asks again, and an hour is a long time to have nothing else to do.
			d.Log.Warn("could not sweep the trash", "error", err)
		}
		if result.Rows > 0 {
			d.Log.Info("purged expired items from the trash",
				"items", result.Items, "rows", result.Rows, "bytes", result.Bytes,
				"retention_days", db.TrashRetentionDays)
		}
	}

	sweep()
	ticker := time.NewTicker(every)
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

// sweepVault removes plaintext an interrupted hide left behind, once at startup
// and on a timer after that.
//
// Cheap by construction: the question is only asked about rows that are already
// in the vault, and the answer is a stat per digest.
func sweepVault(ctx context.Context, vaults *vault.Service, every time.Duration) {
	sweep := func() {
		cleaned, err := vaults.Reconcile(ctx)
		if err != nil {
			return // already logged, and the next tick asks again
		}
		if cleaned > 0 {
			vaults.Log.Warn("swept plaintext left behind by interrupted vault operations",
				"files", cleaned)
		}
	}

	sweep()
	ticker := time.NewTicker(every)
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

// sweepJoinPreviews removes rejected joins that no merge group wants any more.
//
// Every other file under the derivatives root is named after an asset's digest,
// so it is cleaned up by whatever cleans up that asset: a purge removes the
// renditions beside the original, and the vault seals them with it. A join
// preview is named after a merge group — it is what six assets would be if they
// were one — and a group can stop wanting one in ways that never pass through
// this process at all: its parts purged out from under it by the sweep above,
// its question answered by another machine against the same database.
//
// So the tree is reconciled against the database rather than trusted to have
// been tidied. Cheap: one directory walk of a tree already walked once a minute
// for the storage card, against a query that returns single digits.
func sweepJoinPreviews(ctx context.Context, store *db.Store, derivatives *derivstore.Store, log *slog.Logger, every time.Duration) {
	if derivatives == nil {
		return
	}

	sweep := func() {
		on, err := derivatives.Keys(derivstore.JoinPreview)
		if err != nil {
			log.Warn("could not list rejected joins", "error", err)
			return
		}
		if len(on) == 0 {
			return
		}
		wanted, err := store.SegmentPreviews(ctx)
		if err != nil {
			// Without the list there is no way to tell an orphan from evidence
			// somebody is about to look at, and deleting the second is worse
			// than keeping the first.
			log.Warn("could not list the groups a rejected join could belong to", "error", err)
			return
		}

		keep := make(map[string]bool, len(wanted))
		for _, fingerprint := range wanted {
			keep[fingerprint] = true
		}
		removed := 0
		for _, fingerprint := range on {
			if keep[fingerprint] {
				continue
			}
			if err := derivatives.Remove(fingerprint, derivstore.JoinPreview); err != nil {
				log.Warn("could not remove an orphaned join", "error", err, "group", fingerprint)
				continue
			}
			removed++
		}
		if removed > 0 {
			log.Info("removed rejected joins whose groups are gone", "files", removed)
		}
	}

	sweep()
	ticker := time.NewTicker(every)
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
// mediaTools are the external binaries the derivatives are built with, and what
// is lost when one is missing.
//
// One list, read twice: once at startup for the log, and once per request by
// the status page — which re-checks rather than trusting this snapshot, because
// installing the missing package is the obvious fix and nobody should have to
// restart the daemon to see that it worked.
func mediaTools(cfg config.Config) []api.Tool {
	return []api.Tool{
		{Binary: cfg.MagickBin, Needs: "thumbnails and previews"},
		{Binary: cfg.ExiftoolBin, Needs: "capture times, camera, and GPS"},
		{Binary: cfg.FFmpegBin, Needs: "video posters and playback renditions"},
		{Binary: cfg.FFprobeBin, Needs: "video dimensions and duration"},
	}
}

func checkTools(log *slog.Logger, cfg config.Config) {
	for _, tool := range mediaTools(cfg) {
		if _, err := exec.LookPath(tool.Binary); err != nil {
			log.Warn("external tool not found; uploads still work but derivatives will fail",
				"binary", tool.Binary, "affects", tool.Needs)
		}
	}
}
