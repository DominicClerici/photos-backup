// Package config resolves runtime settings from the environment.
package config

import "os"

type Config struct {
	ListenAddr string
	// PhotosRoot holds blobs/ and manifest.jsonl. It is ./data/photos in
	// development and /mnt/photos on the archive machine.
	PhotosRoot  string
	DatabaseURL string
	// MDNSInstance is the advertised service name. Empty means "derive it from
	// the hostname".
	MDNSInstance string
	// MDNSDisabled turns the built-in responder off, which is the escape hatch
	// when the system mDNS daemon will not share port 5353 and the service is
	// published by Avahi instead.
	MDNSDisabled bool
}

func FromEnv() Config {
	return Config{
		ListenAddr:   or(os.Getenv("LISTEN_ADDR"), ":8787"),
		PhotosRoot:   or(os.Getenv("PHOTOS_ROOT"), "./data/photos"),
		DatabaseURL:  or(os.Getenv("DATABASE_URL"), "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"),
		MDNSInstance: os.Getenv("MDNS_INSTANCE"),
		MDNSDisabled: truthy(os.Getenv("MDNS_DISABLED")),
	}
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "YES":
		return true
	}
	return false
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
