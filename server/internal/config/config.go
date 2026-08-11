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
	// already spreads a single clip across several cores.
	TranscodeConcurrency int
	// PreviewConcurrency caps simultaneous on-demand preview conversions, so a
	// fast scroll cannot fork an ImageMagick per request.
	PreviewConcurrency int
	// WorkerDisabled runs photod as a pure API server. The queue still fills;
	// nothing drains it. Useful when the derivative tooling is missing or being
	// upgraded, and it is what a separate photo-worker process would need.
	WorkerDisabled bool

	// VideoEncoder is the ffmpeg encoder for playback renditions. libx264 works
	// everywhere; the archive machine's NVIDIA card can try h264_nvenc.
	VideoEncoder string

	// UploadSessionTTL is how long a partial upload is kept after its last
	// chunk. Long enough to outlast a phone that lost Wi-Fi overnight, short
	// enough that abandoned transfers do not accumulate on the archive drive.
	UploadSessionTTL time.Duration

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

		TLSDir:       os.Getenv("TLS_DIR"),
		TLSExtraSANs: list(os.Getenv("TLS_EXTRA_SANS")),
		TLSDisabled:  truthy(os.Getenv("TLS_DISABLED")),
		// Loopback by default. Widening it is a deliberate choice with a
		// documented consequence: the read path has no authentication yet.
		PlaintextAddr: or(os.Getenv("PLAINTEXT_ADDR"), "127.0.0.1:8788"),

		MDNSInstance: os.Getenv("MDNS_INSTANCE"),
		MDNSDisabled: truthy(os.Getenv("MDNS_DISABLED")),

		WorkerConcurrency:    positive(os.Getenv("WORKER_CONCURRENCY"), 4),
		TranscodeConcurrency: positive(os.Getenv("TRANSCODE_CONCURRENCY"), 1),
		PreviewConcurrency:   positive(os.Getenv("PREVIEW_CONCURRENCY"), 4),
		WorkerDisabled:       truthy(os.Getenv("WORKER_DISABLED")),

		VideoEncoder: or(os.Getenv("VIDEO_ENCODER"), "libx264"),

		UploadSessionTTL: duration(os.Getenv("UPLOAD_SESSION_TTL"), 24*time.Hour),

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
