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
	// PlaintextAddr is a second, unencrypted listener carrying only the
	// gallery's read endpoints and /health — no pairing, no upload path. It is
	// how the Next app and a browser on this machine reach photod without having
	// to trust a private CA. Empty disables it.
	PlaintextAddr string

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
	// VisionConcurrency sizes the pool that hands those renditions to photo-ml.
	// One by default and rarely worth more: it is a queue in front of a single
	// GPU, and a second worker does not make the card faster — it makes two
	// requests wait on it.
	VisionConcurrency int
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
		PlaintextAddr: or(os.Getenv("PLAINTEXT_ADDR"), "127.0.0.1:8788"),

		MDNSInstance: os.Getenv("MDNS_INSTANCE"),
		MDNSDisabled: truthy(os.Getenv("MDNS_DISABLED")),

		WorkerConcurrency:    positive(os.Getenv("WORKER_CONCURRENCY"), 4),
		SignatureConcurrency: positive(os.Getenv("SIGNATURE_CONCURRENCY"), 1),
		TranscodeConcurrency: positive(os.Getenv("TRANSCODE_CONCURRENCY"), 1),
		PrepConcurrency:      positive(os.Getenv("PREP_CONCURRENCY"), 2),
		VisionConcurrency:    positive(os.Getenv("VISION_CONCURRENCY"), 1),
		PreviewConcurrency:   positive(os.Getenv("PREVIEW_CONCURRENCY"), 4),
		WorkerDisabled:       truthy(os.Getenv("WORKER_DISABLED")),

		LivePreviewConcurrency: positive(os.Getenv("LIVE_PREVIEW_CONCURRENCY"), 2),
		LivePreviewCacheBytes:  int64(positive(os.Getenv("LIVE_PREVIEW_CACHE_MB"), 64)) << 20,

		MLURL: strings.TrimSpace(os.Getenv("ML_URL")),

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
