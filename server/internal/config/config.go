// Package config resolves runtime settings from the environment.
package config

import (
	"os"
	"strconv"
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

		MDNSInstance: os.Getenv("MDNS_INSTANCE"),
		MDNSDisabled: truthy(os.Getenv("MDNS_DISABLED")),

		WorkerConcurrency:    positive(os.Getenv("WORKER_CONCURRENCY"), 4),
		TranscodeConcurrency: positive(os.Getenv("TRANSCODE_CONCURRENCY"), 1),
		PreviewConcurrency:   positive(os.Getenv("PREVIEW_CONCURRENCY"), 4),
		WorkerDisabled:       truthy(os.Getenv("WORKER_DISABLED")),

		VideoEncoder: or(os.Getenv("VIDEO_ENCODER"), "libx264"),

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

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
