package manifest

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAppendRoundTripsEntriesInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	log := New(path)
	captured := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)

	first := Entry{
		SHA256: "aaaa", MD5: "bbbb", Size: 1234,
		Filename: "IMG_0001.HEIC", CapturedAt: &captured,
		DeviceID: "iphone-14-pro", LocalID: "local-1",
		StoredAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}
	second := first
	second.SHA256 = "cccc"
	second.Filename = "IMG_0002.HEIC"
	second.LocalID = "local-2"

	if err := log.Append(first); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if err := log.Append(second); err != nil {
		t.Fatalf("append second: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries, want 2", len(got))
	}
	if got[0].SHA256 != "aaaa" || got[1].SHA256 != "cccc" {
		t.Errorf("entries out of order: %q then %q", got[0].SHA256, got[1].SHA256)
	}
	if got[0].Filename != first.Filename || got[0].Size != first.Size {
		t.Errorf("entry round-trip lost fields: %+v", got[0])
	}
	if got[0].CapturedAt == nil || !got[0].CapturedAt.Equal(captured) {
		t.Errorf("CapturedAt = %v, want %v", got[0].CapturedAt, captured)
	}
	if !got[0].StoredAt.Equal(first.StoredAt) {
		t.Errorf("StoredAt = %v, want %v", got[0].StoredAt, first.StoredAt)
	}
}

func TestConcurrentAppendsDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.jsonl")
	log := New(path)
	const writers = 50

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := log.Append(Entry{
				SHA256:   fmt.Sprintf("hash-%02d", i),
				Filename: fmt.Sprintf("IMG_%02d.HEIC", i),
				Size:     int64(i),
				StoredAt: time.Now().UTC(),
			})
			if err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != writers {
		t.Fatalf("read %d entries, want %d", len(got), writers)
	}
	seen := make(map[string]bool, writers)
	for _, e := range got {
		seen[e.SHA256] = true
	}
	for i := range writers {
		if !seen[fmt.Sprintf("hash-%02d", i)] {
			t.Errorf("entry hash-%02d missing after concurrent appends", i)
		}
	}
}
