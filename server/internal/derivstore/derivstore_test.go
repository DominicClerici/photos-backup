package derivstore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// The suffixes are the on-disk contract: a library's derivatives are found by
// name and nothing records what they were called, so a change here orphans every
// file already generated. The base size keeping the bare name is what let the
// other two be added without re-rendering anything.
func TestSuffixesAreTheNamesAlreadyOnDisk(t *testing.T) {
	for _, tc := range []struct {
		size       int
		thumb      string
		liveMotion string
	}{
		{96, ".thumb96.webp", ".live96.mp4"},
		{256, ".thumb.webp", ".live.mp4"},
		{512, ".thumb512.webp", ".live512.mp4"},
	} {
		if got := ThumbSuffix(tc.size); got != tc.thumb {
			t.Errorf("ThumbSuffix(%d) = %q, want %q", tc.size, got, tc.thumb)
		}
		if got := LiveSuffix(tc.size); got != tc.liveMotion {
			t.Errorf("LiveSuffix(%d) = %q, want %q", tc.size, got, tc.liveMotion)
		}
		if !IsThumbSize(tc.size) {
			t.Errorf("IsThumbSize(%d) = false, but it has a suffix", tc.size)
		}
	}

	if IsThumbSize(97) || IsThumbSize(0) {
		t.Error("IsThumbSize accepted a size nothing renders")
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

func TestUsageChargesEachRenditionToItsOriginal(t *testing.T) {
	s := New(t.TempDir())

	const photo = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const clip = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const gone = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	write := func(sha, suffix string, n int) {
		t.Helper()
		if err := s.Write(sha, suffix, func(w io.Writer) error {
			_, err := w.Write(make([]byte, n))
			return err
		}); err != nil {
			t.Fatalf("write %s%s: %v", sha, suffix, err)
		}
	}
	write(photo, Thumb, 100)
	write(photo, ThumbSuffix(512), 400)
	// A video's poster frame is a .webp like any other, which is the whole
	// reason Usage asks about the original rather than reading the suffix.
	write(clip, Thumb, 100)
	write(clip, Playback, 5_000)
	write(gone, Thumb, 70)

	kinds := map[string]string{photo: "image", clip: "video"}
	usage, err := s.Usage(func(sha string) string { return kinds[sha] })
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage["image"] != 500 {
		t.Errorf("image bytes = %d, want 500", usage["image"])
	}
	if usage["video"] != 5_100 {
		t.Errorf("video bytes = %d, want 5100 (the poster counts as the video's)", usage["video"])
	}
	// A rendition whose original was purged or hidden is still on the disk, so
	// it is reported rather than dropped.
	if usage[""] != 70 {
		t.Errorf("unattributed bytes = %d, want 70", usage[""])
	}
}

func TestUsageIgnoresTheVaultAndTheStagingArea(t *testing.T) {
	s := New(t.TempDir())

	const kept = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if err := s.Write(kept, Thumb, func(w io.Writer) error {
		_, err := w.Write(make([]byte, 10))
		return err
	}); err != nil {
		t.Fatalf("write thumbnail: %v", err)
	}

	staged, _, err := s.Stage("half-a-transcode")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := os.WriteFile(staged, make([]byte, 9_000), 0o644); err != nil {
		t.Fatalf("fill staging file: %v", err)
	}

	sealed := filepath.Join(s.Root(), "vault", "de", "ad", kept+Thumb+".age")
	if err := os.MkdirAll(filepath.Dir(sealed), 0o755); err != nil {
		t.Fatalf("create vault tree: %v", err)
	}
	if err := os.WriteFile(sealed, make([]byte, 8_000), 0o644); err != nil {
		t.Fatalf("write sealed rendition: %v", err)
	}

	usage, err := s.Usage(func(string) string { return "image" })
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage["image"] != 10 {
		t.Errorf("image bytes = %d, want 10; the staging file or the vault was counted", usage["image"])
	}
}

// Keys is what the sweep for orphaned joins reads. It has to find every file of
// one suffix and nothing else — a thumbnail counted as a rejected join would be
// deleted as one.
func TestKeysFindsOneSuffixAndNoOther(t *testing.T) {
	s := New(t.TempDir())

	const a = "1111111111111111111111111111111111111111111111111111111111111111"
	const b = "2222222222222222222222222222222222222222222222222222222222222222"
	write := func(sha, suffix string) {
		t.Helper()
		if err := s.Write(sha, suffix, func(w io.Writer) error {
			_, err := w.Write([]byte("x"))
			return err
		}); err != nil {
			t.Fatalf("write %s%s: %v", sha, suffix, err)
		}
	}
	write(a, JoinPreview)
	write(b, JoinPreview)
	write(a, Thumb)
	// The one name that could be mistaken for a join preview by a suffix test
	// alone: a video's playback rendition, whose suffix .mp4 ends the same way.
	write(b, Playback)

	keys, err := s.Keys(JoinPreview)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{a, b}) {
		t.Errorf("Keys = %v, want exactly the two join previews", keys)
	}

	// And an empty tree is an archive with nothing kept, not a failure.
	empty, err := New(t.TempDir()).Keys(JoinPreview)
	if err != nil || len(empty) != 0 {
		t.Errorf("Keys on an empty store = %v, %v; want none and no error", empty, err)
	}
}
