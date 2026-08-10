// Package config resolves runtime settings from the environment.
package config

import "os"

type Config struct {
	ListenAddr string
	// PhotosRoot holds blobs/ and manifest.jsonl. It is ./data/photos in
	// development and /mnt/photos on the archive machine.
	PhotosRoot  string
	DatabaseURL string
}

func FromEnv() Config {
	return Config{
		ListenAddr:  or(os.Getenv("LISTEN_ADDR"), ":8787"),
		PhotosRoot:  or(os.Getenv("PHOTOS_ROOT"), "./data/photos"),
		DatabaseURL: or(os.Getenv("DATABASE_URL"), "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"),
	}
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
