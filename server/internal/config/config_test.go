package config

import "testing"

func TestFromEnvUsesLocalDevelopmentDefaults(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PHOTOS_ROOT", "")
	t.Setenv("DATABASE_URL", "")

	cfg := FromEnv()

	if cfg.ListenAddr != ":8787" {
		t.Errorf("ListenAddr = %q, want :8787", cfg.ListenAddr)
	}
	if cfg.PhotosRoot != "./data/photos" {
		t.Errorf("PhotosRoot = %q, want ./data/photos", cfg.PhotosRoot)
	}
	if cfg.DatabaseURL == "" {
		t.Error("DatabaseURL default is empty")
	}
}

func TestFromEnvPrefersEnvironment(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("PHOTOS_ROOT", "/mnt/photos")
	t.Setenv("DATABASE_URL", "postgres://elsewhere/db")

	cfg := FromEnv()

	if cfg.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q, want :9000", cfg.ListenAddr)
	}
	if cfg.PhotosRoot != "/mnt/photos" {
		t.Errorf("PhotosRoot = %q, want /mnt/photos", cfg.PhotosRoot)
	}
	if cfg.DatabaseURL != "postgres://elsewhere/db" {
		t.Errorf("DatabaseURL = %q, want the environment value", cfg.DatabaseURL)
	}
}

func TestWorkerDefaultsFavorThumbnailsOverTranscodes(t *testing.T) {
	cfg := FromEnv()

	// The gallery is blocked on metadata work and not on transcodes, so the
	// pools must not be sized the same.
	if cfg.WorkerConcurrency <= cfg.TranscodeConcurrency {
		t.Errorf("WorkerConcurrency = %d, TranscodeConcurrency = %d; metadata should get more",
			cfg.WorkerConcurrency, cfg.TranscodeConcurrency)
	}
	if cfg.VideoEncoder != "libx264" {
		t.Errorf("VideoEncoder = %q, want libx264 as the portable default", cfg.VideoEncoder)
	}
}

// A typo in a count must not start a server with zero workers, which would look
// exactly like a queue that has silently stopped.
func TestConcurrencyFallsBackOnUnusableValues(t *testing.T) {
	for _, value := range []string{"", "0", "-3", "four"} {
		t.Setenv("WORKER_CONCURRENCY", value)
		if got := FromEnv().WorkerConcurrency; got != 4 {
			t.Errorf("WORKER_CONCURRENCY=%q gave %d, want the default 4", value, got)
		}
	}
}

// Empty means "beside the originals"; main resolves it. Keeping the empty value
// here is what lets main tell "unset" apart from "deliberately set to the same
// place".
func TestDerivativesRootDefaultsToEmptyForMainToResolve(t *testing.T) {
	t.Setenv("DERIVATIVES_ROOT", "")
	if got := FromEnv().DerivativesRoot; got != "" {
		t.Errorf("DerivativesRoot = %q, want empty", got)
	}

	t.Setenv("DERIVATIVES_ROOT", "/var/lib/photobackup/derivatives")
	if got := FromEnv().DerivativesRoot; got != "/var/lib/photobackup/derivatives" {
		t.Errorf("DerivativesRoot = %q", got)
	}
}
