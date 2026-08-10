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
