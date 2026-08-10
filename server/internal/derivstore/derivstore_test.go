package derivstore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "abcd1234ef567890abcd1234ef567890abcd1234ef567890abcd1234ef567890"

func TestPathFansOutOnTheDigest(t *testing.T) {
	s := New("/mnt/derivatives")

	got := s.Path(sha, Thumb)
	want := filepath.Join("/mnt/derivatives", "ab", "cd", sha+".thumb.webp")
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestWriteCommitsWhatItProduced(t *testing.T) {
	s := New(t.TempDir())

	if err := s.Write(sha, Thumb, func(w io.Writer) error {
		_, err := io.WriteString(w, "webp bytes")
		return err
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(s.Path(sha, Thumb))
	if err != nil {
		t.Fatalf("read committed derivative: %v", err)
	}
	if string(got) != "webp bytes" {
		t.Errorf("committed %q, want %q", got, "webp bytes")
	}
	if !s.Exists(sha, Thumb) {
		t.Error("Exists = false after a successful write")
	}
}

// A failed conversion must not leave something servable behind. Half a
// thumbnail renders as a broken image, which looks like data loss.
func TestWriteLeavesNothingBehindWhenTheProducerFails(t *testing.T) {
	s := New(t.TempDir())
	boom := errors.New("magick: no decode delegate")

	err := s.Write(sha, Thumb, func(w io.Writer) error {
		io.WriteString(w, "partial output before the failure")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write error = %v, want the producer's error", err)
	}

	if s.Exists(sha, Thumb) {
		t.Error("a failed write committed a derivative anyway")
	}
	if _, err := os.Stat(s.Path(sha, Thumb)); !os.IsNotExist(err) {
		t.Errorf("destination exists after a failed write: %v", err)
	}
	assertStagingEmpty(t, s)
}

// Re-running a job has to be safe: the queue retries, and a lease reclaim can
// run a job whose previous attempt already wrote its output.
func TestWriteReplacesAnExistingDerivative(t *testing.T) {
	s := New(t.TempDir())

	for _, content := range []string{"first pass", "second pass"} {
		if err := s.Write(sha, Thumb, func(w io.Writer) error {
			_, err := io.WriteString(w, content)
			return err
		}); err != nil {
			t.Fatalf("Write %q: %v", content, err)
		}
	}

	got, err := os.ReadFile(s.Path(sha, Thumb))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second pass" {
		t.Errorf("content = %q, want the rerun's output", got)
	}
}

func TestStageCommitMovesTheFileIntoPlace(t *testing.T) {
	s := New(t.TempDir())

	staged, cleanup, err := s.Stage("transcode-*.mp4")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(staged, filepath.Join(s.Root(), "tmp")) {
		t.Errorf("staged at %q, want it inside the staging dir", staged)
	}
	if err := os.WriteFile(staged, []byte("mp4 bytes"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	if err := s.Commit(sha, Playback, staged); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(s.Path(sha, Playback))
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if string(got) != "mp4 bytes" {
		t.Errorf("committed %q", got)
	}
	assertStagingEmpty(t, s)
}

func TestExistsRejectsAnEmptyFile(t *testing.T) {
	s := New(t.TempDir())

	path := s.Path(sha, Thumb)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	// A zero-byte derivative is a failed conversion, not a valid rendition.
	if s.Exists(sha, Thumb) {
		t.Error("Exists = true for a zero-byte file")
	}
}

func TestRemoveToleratesAMissingFile(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Remove(sha, Thumb); err != nil {
		t.Errorf("Remove on a missing derivative: %v", err)
	}
}

func assertStagingEmpty(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("staging dir holds %d leftover files", len(entries))
	}
}
