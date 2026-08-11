// Package derivstore holds generated renditions on disk: thumbnails and video
// playback files, addressed by the SHA-256 of the original they came from.
//
// It deliberately mirrors blobstore's layout and differs in exactly one way:
// nothing here is fsynced. A blob is irreplaceable and pays for durability on
// every write. A derivative can be rebuilt from its blob by re-running one job,
// so paying fsync on every thumbnail through a 40,000-item backfill buys
// nothing. Writes are still staged and renamed, so a torn file is impossible —
// only a crash-forgotten one, which the queue notices and redoes.
package derivstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Suffixes appended to the digest. All three are part of the on-disk contract,
// so changing one orphans every derivative already generated.
const (
	Thumb    = ".thumb.webp"
	Playback = ".mp4"
	// LiveThumb is a Live Photo's three seconds at thumbnail size, cropped
	// square to sit exactly where the still it replaces sat. It is the only
	// rendition a paired video keeps: the viewer's larger one is rendered per
	// request, the way a photo's 2048px preview is.
	LiveThumb = ".live.mp4"
)

// ThumbSizes are the square edge lengths every still is rendered at, smallest
// first, and the sizes a Live Photo's motion is rendered at beside it. The
// gallery picks one per zoom level: 96 for the two smallest cells, 512 for the
// two largest, and the base everywhere between.
//
// Three sizes rather than one because the extremes are both wasteful in the
// same way. A 256px file drawn in a 64px cell is fifteen times the pixels the
// screen will use, times a screenful of tiles; the same file stretched into a
// 512px cell is the one place the grid visibly softens.
var ThumbSizes = []int{96, 256, 512}

// BaseThumbSize is the size everything else falls back to, and the only one
// whose files carry an unadorned suffix.
//
// The bare name is not tidiness, it is the reason this change did not have to
// re-render an existing library: every `.thumb.webp` and `.live.mp4` already on
// disk is still exactly what it claims to be, and only the two new sizes have
// to be built. The API's plain /thumb and /live/thumb routes answer from these
// same files, so a client that knows nothing about sizes is unaffected.
const BaseThumbSize = 256

// ThumbSuffix names the still rendition of a given size.
func ThumbSuffix(size int) string {
	if size == BaseThumbSize {
		return Thumb
	}
	return fmt.Sprintf(".thumb%d.webp", size)
}

// LiveSuffix names the motion rendition of a given size.
func LiveSuffix(size int) string {
	if size == BaseThumbSize {
		return LiveThumb
	}
	return fmt.Sprintf(".live%d.mp4", size)
}

// IsThumbSize reports whether a size is one this archive stores. It exists so
// the API can reject `/thumb/97` before it turns into a path on disk.
func IsThumbSize(size int) bool {
	for _, s := range ThumbSizes {
		if s == size {
			return true
		}
	}
	return false
}

type Store struct {
	root string
}

func New(root string) *Store { return &Store{root: root} }

func (s *Store) Root() string { return s.root }

// Path is a pure function of the digest and suffix, so it stays valid across
// restarts and never needs a database lookup.
func (s *Store) Path(sha256hex, suffix string) string {
	return filepath.Join(s.root, sha256hex[0:2], sha256hex[2:4], sha256hex+suffix)
}

func (s *Store) Exists(sha256hex, suffix string) bool {
	info, err := os.Stat(s.Path(sha256hex, suffix))
	return err == nil && info.Size() > 0
}

func (s *Store) Open(sha256hex, suffix string) (*os.File, error) {
	return os.Open(s.Path(sha256hex, suffix))
}

// Stage creates an empty file in the staging directory and returns its path
// along with a cleanup that removes it. It exists for tools like ffmpeg that
// need a real seekable path to write to — `-movflags +faststart` rewrites the
// file's header at the end, which a pipe cannot do.
//
// The caller must call cleanup; Commit leaves nothing behind for it to remove.
func (s *Store) Stage(name string) (path string, cleanup func(), err error) {
	dir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create staging dir: %w", err)
	}
	f, err := os.CreateTemp(dir, name)
	if err != nil {
		return "", func() {}, fmt.Errorf("create staging file: %w", err)
	}
	path = f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", func() {}, fmt.Errorf("close staging file: %w", err)
	}
	return path, func() { os.Remove(path) }, nil
}

// Commit moves a staged file into its final location, replacing whatever was
// there. Replacing rather than refusing is what makes a re-run of a job safe:
// the output is a deterministic function of the blob, so rewriting it is a
// no-op in content even when the bytes differ slightly between encoder builds.
func (s *Store) Commit(sha256hex, suffix, staged string) error {
	dst := s.Path(sha256hex, suffix)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create derivative dir: %w", err)
	}
	if err := os.Rename(staged, dst); err != nil {
		return fmt.Errorf("commit derivative: %w", err)
	}
	return nil
}

// Write stages whatever fn produces and commits it. This is the path for tools
// that stream to stdout, which is most of them.
//
// A failing fn leaves the staging file behind for cleanup and never touches the
// destination, so a half-written thumbnail cannot be served.
func (s *Store) Write(sha256hex, suffix string, fn func(io.Writer) error) error {
	staged, cleanup, err := s.Stage("derive-*")
	if err != nil {
		return err
	}
	defer cleanup()

	f, err := os.OpenFile(staged, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open staging file: %w", err)
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staging file: %w", err)
	}
	return s.Commit(sha256hex, suffix, staged)
}

// Remove deletes a derivative if it exists. A missing file is not an error:
// the point is to reach a state, not to assert the previous one.
func (s *Store) Remove(sha256hex, suffix string) error {
	err := os.Remove(s.Path(sha256hex, suffix))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove derivative: %w", err)
	}
	return nil
}
