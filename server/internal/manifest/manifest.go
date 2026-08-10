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
	SHA256   string `json:"sha256"`
	MD5      string `json:"md5"`
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
	// ContentType and Ext are what the file was classified as on the way in.
	// Ext is also the blob's extension on disk, so a rebuild can find the blob
	// from the manifest alone rather than guessing at the filename — which for
	// an extensionless Takeout video would guess wrong.
	//
	// Both are absent from lines written before classification moved out of the
	// filename; a rebuild re-derives them from the blob's own bytes.
	ContentType string     `json:"content_type,omitempty"`
	Ext         string     `json:"ext,omitempty"`
	CapturedAt  *time.Time `json:"captured_at,omitempty"`
	// ModifiedAt is the local asset's modification time on the source device. It
	// belongs here so a database rebuilt from this log can restore the device
	// mappings complete, rather than forcing every asset through a content check.
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
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

// Read parses the whole log into memory. Convenient for tests and small
// archives; verify and reindex use Scan instead, because a 100GB library's log
// is hundreds of thousands of lines and there is no reason to hold them all.
func Read(path string) ([]Entry, error) {
	var entries []Entry
	err := Scan(path, func(e Entry) error {
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Scan calls fn once per entry, in the order they were appended.
//
// A parse failure names the line rather than aborting silently, and stops the
// scan: a log that has gone unreadable partway through is a fact worth
// surfacing, not one to skip past on the way to a "clean" result.
func Scan(path string, fn func(Entry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for scanner.Scan() {
		line++
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("parse manifest line %d: %w", line, err)
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	return nil
}
