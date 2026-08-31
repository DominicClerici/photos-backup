// Package config resolves runtime settings from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	// PhotosRoot holds blobs/ and manifest.jsonl. It is ./data/photos in
	// development and /mnt/photos on the archive machine.
	PhotosRoot string
	// DerivativesRoot holds thumbnails and playback renditions. Empty means
	// PhotosRoot/derivatives.
	//
	// It is a separate setting because the two have opposite requirements:
	// originals want the big slow drive and can never be regenerated, while
	// derivatives want the SSD the gallery is served from and can always be
	// rebuilt. On the archive machine they belong on different disks.
	DerivativesRoot string
	DatabaseURL     string

	// GeoNamesDir holds the offline geocoder's extract: cities500, the admin1
	// codes and the country names, exactly as downloaded. See internal/geocode
	// for what goes in it and where from.
	//
	// Its own setting rather than a directory under PhotosRoot because it is
	// neither part of the library nor derived from it. It is a reference table
	// that can be re-downloaded, which puts it with the machine's state on the
	// SSD rather than on the archive drive. Photographs keep their coordinates
	// and go without a place name when it is absent.
	GeoNamesDir string

	// TLSDir holds ca.crt, ca.key and the server certificate photod issues for
	// itself. Empty means PhotosRoot/tls.
	//
	// Worth moving off the archive drive on a real deployment: ca.key is machine
	// state, not part of the library, and it is the one file whose loss means
	// re-pairing every device.
	TLSDir string
	// TLSExtraSANs are additional names or addresses to certify, beyond the ones
	// photod can see from the inside. Comma-separated.
	TLSExtraSANs []string
	// TLSDisabled serves the API in the clear on ListenAddr, tokens and all.
	// Development only, and photod says so loudly at startup.
	TLSDisabled bool
	// WebOrigin is the address a browser reaches this archive at, e.g.
	// https://archive.tail1234.ts.net. Empty disables browser sign-in entirely:
	// the sign-in endpoints answer 503 and only paired devices can read the
	// archive.
	//
	// It is not a cosmetic setting. A passkey is bound to the origin it was
	// registered under — that binding is what makes it unphishable — so this
	// value is baked into every credential this archive holds. Changing it means
	// every registered passkey stops resolving and has to be registered again,
	// which is why it is configured explicitly rather than sniffed from the Host
	// header of whatever request happens to arrive.
	WebOrigin string
	// WebAppURL is where the Next process is listening, e.g.
	// http://127.0.0.1:3000. photod reverse-proxies it, so the gallery, the JSON
	// and the thumbnails all arrive from one origin under one cookie — the
	// constraint PROJECT.md records from Phase 12, since a browser attaches a
	// same-origin cookie to an <img> and will not attach a bearer header to one.
	//
	// Empty means photod serves the API and no gallery, which is the right
	// degradation while the Next process is being deployed: the phone keeps
	// backing up.
	WebAppURL string
	// WebIdle ends a browser session that has not been used, and WebLifetime is
	// the absolute cap that nothing resets. A session dies at whichever comes
	// first.
	WebIdle     time.Duration
	WebLifetime time.Duration

	// MDNSInstance is the advertised service name. Empty means "derive it from
	// the hostname".
	MDNSInstance string
	// MDNSDisabled turns the built-in responder off, which is the escape hatch
	// when the system mDNS daemon will not share port 5353 and the service is
	// published by Avahi instead.
	MDNSDisabled bool

	// WorkerConcurrency sizes the metadata pool: exiftool and thumbnails. The
	// gallery is blocked on this work, so it gets the parallelism.
	WorkerConcurrency int
	// TranscodeConcurrency sizes the video pool. One by default, because ffmpeg
	// already spreads a single clip across several cores — measured, a second
	// libx264 worker is worth a few percent on long clips. It is worth
	// considerably more on the three-second Live Photo sidecars that dominate
	// the queue by count, where one clip cannot fill the machine; the archive
	// host raises this to 4 in its env file.
	TranscodeConcurrency int
	// SignatureConcurrency sizes the pool that reduces originals to the numbers
	// the duplicate scan compares. One by default, and deliberately the
	// smallest of the three: it decodes every original in the archive and
	// samples twenty frames out of every video, and nothing at all is waiting
	// for the answer. A backfill should be something the machine does in the
	// background over an hour, not something it does instead of serving the
	// gallery.
	SignatureConcurrency int
	// PrepConcurrency sizes the pool that writes the renditions a vision model
	// reads. Two by default: it is another full decode of every visible
	// original in the archive, and like the signature pass nothing is waiting
	// for it — but unlike the signature pass each item is one ImageMagick
	// rather than twenty sampled frames, so a second worker is worth having.
	PrepConcurrency int
	// VisionConcurrency sizes the pool that hands those renditions to photo-ml
	// — embeddings, recognised text and captions, in that order of priority.
	//
	// Four by default, and it used to be one. The old reasoning was sound and
	// is now obsolete: a queue in front of a single GPU gains nothing from a
	// second worker, because two requests just wait on the same card. What
	// changed is that photo-ml learned to batch. The captioner is
	// memory-bandwidth bound — 8.8GB of weights read per generated token,
	// whether that token is for one image or twelve — so a batch of four costs
	// barely more than one and halves the wall clock of an overnight pass. That
	// batch can only form if several requests are in flight, and this is what
	// puts them there. See photo-ml/src/photo_ml/batching.py.
	//
	// 4 is also exactly one describe batch, so raising it no longer speeds that
	// pass up: PHOTO_ML_DESCRIBE_BATCH is 4 — a batch of 8 does not fit at the
	// captioner's pixel budget — and the card is the limit, not anything on
	// this side.
	VisionConcurrency int
	// DescribeQuietSeconds is how long the embedding and text-recognition
	// queues must have been quiet before the captioner is allowed onto the
	// card. See worker.visionHold: the two cheap passes and the expensive one
	// cannot share a 16GB card, and an empty queue is not the same question as
	// a quiet one when a phone is uploading a photograph every two seconds.
	//
	// Two minutes because it is comfortably longer than a burst of uploads is
	// spaced and comfortably shorter than anybody's patience for a caption.
	// Nothing else waits on it: a new photograph is searchable by its text and
	// by what it looks like within seconds either way.
	DescribeQuietSeconds int
	// PreviewConcurrency caps simultaneous on-demand preview conversions, so a
	// fast scroll cannot fork an ImageMagick per request.
	PreviewConcurrency int
	// LivePreviewConcurrency is the same cap for Live Photo renditions, and
	// lower: an ffmpeg costs considerably more than an ImageMagick.
	LivePreviewConcurrency int
	// LivePreviewCacheBytes bounds the memory those renditions are held in
	// between requests. They are stored nowhere else, so this is the only place
	// a repeated view is cheaper than the first.
	LivePreviewCacheBytes int64
	// WorkerDisabled runs photod as a pure API server. The queue still fills;
	// nothing drains it. Useful when the derivative tooling is missing or being
	// upgraded, and it is what a separate photo-worker process would need.
	WorkerDisabled bool

	// MLURL is where photo-ml is listening, e.g. http://127.0.0.1:8789. Empty
	// means there is none, and empty is a supported way to run this archive
	// forever: PROJECT.md §4's hard rule is that photo-ml is optional, so the
	// default is off and turning it on is a deliberate line in an env file.
	//
	// Without it the vision pool is not started and no vision work is queued —
	// not "queued and never drained", which would leave a machine with no GPU
	// service reporting a permanent seventeen-thousand-item backlog for a
	// feature it does not have. Setting this and restarting is what turns the
	// whole library into queued work. See jobs.ReconcileVision.
	MLURL string

	// MLSearchIdle is how long photo-ml keeps the search models on the card
	// after the last gallery request.
	//
	// The number this whole arrangement turns on. Those two models used to be
	// resident — loaded when photo-ml started and never given back — so an
	// archive nobody was looking at held about 3GB of weights and a CUDA
	// context all day for a search box nobody had open. photod is the only
	// process that can see a browser, so photod is what says otherwise: it
	// takes a lease on a page load and pushes it forward on ordinary gallery
	// traffic, and this is how far forward.
	//
	// Five minutes because it is comfortably longer than reading one page and
	// comfortably shorter than holding the card all evening for a tab that was
	// closed. It is a term rather than a timer: photo-ml lets go on its own if
	// photod stops asking, so a restart here costs the next search a cold load
	// and nothing else.
	MLSearchIdle time.Duration
	// MLIngestRetry is how long the vision pool waits before asking again after
	// photo-ml refuses it the card.
	//
	// Refusals are ordinary and expected — somebody has the gallery open, or a
	// game is holding nine gigabytes — so this is a pace rather than a backoff,
	// and it does not grow. Fifteen minutes because the two things it is
	// waiting on move on that scale: a browsing session ends, or something else
	// on the card finishes. Nothing is lost by waiting, because the queue is
	// the state and the work is still in it.
	MLIngestRetry time.Duration

	// VideoEncoder is the ffmpeg encoder for playback renditions. libx264 works
	// everywhere; the archive machine's NVIDIA card runs h264_nvenc, which
	// video.videoArgs knows how to configure. Anything named *nvenc takes the
	// hardware path there, including hevc_nvenc.
	VideoEncoder string

	// UploadSessionTTL is how long a partial upload is kept after its last
	// chunk. Long enough to outlast a phone that lost Wi-Fi overnight, short
	// enough that abandoned transfers do not accumulate on the archive drive.
	UploadSessionTTL time.Duration

	// PurgeInterval is how often the trash is swept for items that have served
	// their retention. Frequent is cheap — the question is one index probe and
	// the answer is almost always "nothing" — and the resolution it buys is not
	// worth much either way against a year-long window.
	PurgeInterval time.Duration
	// PurgeDisabled stops the sweep entirely. The trash then grows without
	// bound, which is a coherent thing to want on a machine where nothing
	// should ever delete an original without somebody typing.
	PurgeDisabled bool

	// VaultIdle is how long the Archive and Hidden buckets stay unlocked
	// without being used. See vault.DefaultIdle for why fifteen minutes.
	//
	// The encrypted trees themselves are not configurable: they are `vault/`
	// under PhotosRoot and under DerivativesRoot, so the same "originals on the
	// slow disk, renditions on the SSD" split holds on both sides of the lock.
	VaultIdle time.Duration

	// Binary overrides for hosts where the tools are not on PATH.
	MagickBin   string
	FFmpegBin   string
	FFprobeBin  string
	ExiftoolBin string
}

func FromEnv() Config {
	return Config{
		ListenAddr:      or(os.Getenv("LISTEN_ADDR"), ":8787"),
		PhotosRoot:      or(os.Getenv("PHOTOS_ROOT"), "./data/photos"),
		DerivativesRoot: os.Getenv("DERIVATIVES_ROOT"),
		DatabaseURL:     or(os.Getenv("DATABASE_URL"), "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"),

		GeoNamesDir: or(os.Getenv("GEONAMES_DIR"), "./data/geonames"),

		TLSDir:       os.Getenv("TLS_DIR"),
		TLSExtraSANs: list(os.Getenv("TLS_EXTRA_SANS")),
		TLSDisabled:  truthy(os.Getenv("TLS_DISABLED")),
		// Loopback by default. Widening it is a deliberate choice with a
		// documented consequence: the read path has no authentication yet.
		WebOrigin:   strings.TrimRight(os.Getenv("WEB_ORIGIN"), "/"),
		WebAppURL:   or(os.Getenv("WEB_APP_URL"), "http://127.0.0.1:3000"),
		WebIdle:     duration(os.Getenv("WEB_IDLE"), time.Hour),
		WebLifetime: duration(os.Getenv("WEB_LIFETIME"), 12*time.Hour),

		MDNSInstance: os.Getenv("MDNS_INSTANCE"),
		MDNSDisabled: truthy(os.Getenv("MDNS_DISABLED")),

		WorkerConcurrency:    positive(os.Getenv("WORKER_CONCURRENCY"), 4),
		SignatureConcurrency: positive(os.Getenv("SIGNATURE_CONCURRENCY"), 1),
		TranscodeConcurrency: positive(os.Getenv("TRANSCODE_CONCURRENCY"), 1),
		PrepConcurrency:      positive(os.Getenv("PREP_CONCURRENCY"), 2),
		VisionConcurrency:    positive(os.Getenv("VISION_CONCURRENCY"), 4),
		DescribeQuietSeconds: positive(os.Getenv("DESCRIBE_QUIET_SECONDS"), 120),
		PreviewConcurrency:   positive(os.Getenv("PREVIEW_CONCURRENCY"), 4),
		WorkerDisabled:       truthy(os.Getenv("WORKER_DISABLED")),

		LivePreviewConcurrency: positive(os.Getenv("LIVE_PREVIEW_CONCURRENCY"), 2),
		LivePreviewCacheBytes:  int64(positive(os.Getenv("LIVE_PREVIEW_CACHE_MB"), 64)) << 20,

		MLURL:         strings.TrimSpace(os.Getenv("ML_URL")),
		MLSearchIdle:  duration(os.Getenv("ML_SEARCH_IDLE"), 5*time.Minute),
		MLIngestRetry: duration(os.Getenv("ML_INGEST_RETRY"), 15*time.Minute),

		VideoEncoder: or(os.Getenv("VIDEO_ENCODER"), "libx264"),

		UploadSessionTTL: duration(os.Getenv("UPLOAD_SESSION_TTL"), 24*time.Hour),

		VaultIdle: duration(os.Getenv("VAULT_IDLE"), 15*time.Minute),

		PurgeInterval: duration(os.Getenv("PURGE_INTERVAL"), time.Hour),
		PurgeDisabled: truthy(os.Getenv("PURGE_DISABLED")),

		MagickBin:   or(os.Getenv("MAGICK_BIN"), "magick"),
		FFmpegBin:   or(os.Getenv("FFMPEG_BIN"), "ffmpeg"),
		FFprobeBin:  or(os.Getenv("FFPROBE_BIN"), "ffprobe"),
		ExiftoolBin: or(os.Getenv("EXIFTOOL_BIN"), "exiftool"),
	}
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "YES":
		return true
	}
	return false
}

// positive falls back on anything that is not a usable count, so a typo in an
// env var degrades to the default rather than starting a server with no workers.
func positive(v string, fallback int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// duration falls back on anything unparseable or non-positive, for the same
// reason positive does: a typo should cost the default, not the daemon.
func duration(v string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// list splits a comma-separated setting, dropping blanks so a trailing comma is
// not a configuration error.
func list(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
