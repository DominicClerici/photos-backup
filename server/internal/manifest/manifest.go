// Package manifest maintains the append-only recovery log that sits beside the
// blobs. It is the disaster-recovery path when the database is gone, so it is
// written independently of Postgres and never rewritten in place.
package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one line of manifest.jsonl: everything needed to rebuild a database
// row from the blob tree alone.
type Entry struct {
	SHA256     string     `json:"sha256"`
	MD5        string     `json:"md5"`
	Size       int64      `json:"size"`
	Filename   string     `json:"filename"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	DeviceID   string     `json:"device_id"`
	LocalID    string     `json:"local_id"`
	StoredAt   time.Time  `json:"stored_at"`
}

type Log struct {
	mu   sync.Mutex
	path string
}

func New(path string) *Log {
	return &Log{path: path}
}

// Append writes one entry and fsyncs before returning, so a caller that has
// seen a nil error knows the line survives a crash.
func (l *Log) Append(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal manifest entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write manifest entry: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync manifest: %w", err)
	}
	return nil
}

// Read parses the whole log. Phase 1 uses it only in tests; the rebuild path in
// a later phase is the real consumer.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("parse manifest line: %w", err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return entries, nil
}
