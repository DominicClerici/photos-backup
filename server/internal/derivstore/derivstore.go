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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Suffixes appended to the digest. Every one of them is part of the on-disk
// contract, so changing one orphans every derivative already generated.
const (
	Thumb    = ".thumb.webp"
	Playback = ".mp4"
	// PlaybackPlain is the same rendition of a Snapchat memory's video without
	// its caption layer burned in. It exists only for the few hundred videos
	// that carry one: everything else has nothing to leave out, and the toggle
	// that reaches for this falls back to the ordinary playback when it is
	// absent.
	PlaybackPlain = ".plain.mp4"
	// LiveThumb is a Live Photo's three seconds at thumbnail size, cropped
	// square to sit exactly where the still it replaces sat. It is the only
	// rendition a paired video keeps: the viewer's larger one is rendered per
	// request, the way a photo's 2048px preview is.
	LiveThumb = ".live.mp4"
	// JoinPreview is a joined recording the merge worker built and then refused
	// to archive, kept so that somebody can watch it and say whether it is
	// right after all. See video.DurationMismatch.
	//
	// The one file in this tree that is not a rendition of an original, and the
	// one keyed by something other than an asset's digest: its name is the
	// merge group's fingerprint, because the thing it is a picture of is a
	// group rather than an asset. That makes it invisible to RemoveAll, which
	// works per asset — the group is what owns it, and db.SegmentPreviews plus
	// the sweep in photod are what stop it outliving one.
	JoinPreview = ".join.mp4"
	// MLStill is what a vision model reads: the whole photograph, uncropped, at
	// MLEdge on its longest side. One file for all three passes — the encoder,
	// the captioner and the text recogniser — because MLEdge is sized for the
	// hungriest of them and the other two cap their own input anyway.
	//
	// Uncropped is the entire point, and it is why this is a fourth rendition
	// rather than a reuse of .thumb512.webp. That file is a square centre crop,
	// which is exactly right for a grid of fixed square cells and exactly wrong
	// here — the dog is often at the edge, and a centre crop is a machine
	// deciding in advance what the photograph was about.
	//
	// It is also what keeps the model out of the archive. photo-ml receives
	// these bytes over loopback and never opens /mnt/photos, so the vault is
	// excluded by construction rather than by a WHERE clause somebody can
	// forget to write. See ML_IMAGES.md §3.
	MLStill = ".ml.webp"
)

// MLEdge bounds the ML rendition's longest side.
//
// This was 512, on the reasoning that it was comfortably above what the
// encoders in question actually see — a SigLIP-class model resizes its input to
// 384 or 448 — so storing them larger than any candidate needs bought nothing.
// The first half of that is still true and the conclusion was wrong, because a
// text recogniser is a candidate and it is the one model here that wants every
// pixel of the frame it can get. Measured against known text on a 4032x3024
// photograph, the share of the frame a line of text occupies against whether it
// could be read at all:
//
//	text height   512    1536      what a line that size is
//	2.1% frame   1.00    1.00      a road sign, a shopfront, a headline
//	1.2% frame   0.42    1.00      a receipt held at arm's length
//	0.9% frame   0.00    1.00      body text in a screenshot, a menu
//
// Those bottom two rows are receipts, menus, whiteboards, book pages and the
// body of every dense screenshot — the photographs where recognised text is the
// only thing that could ever make them findable, because no caption will
// contain a confirmation number.
//
// 1536 rather than more because the recogniser's own preprocessing caps its
// input at a 2000px side, so anything past that is bytes nobody reads.
//
// It costs the encoder nothing, which is the fact that made one rendition
// possible instead of two: SigLIP hard-resizes to 384x384 and cannot tell.
//
// The captioner is a different story now and this comment used to have it
// backwards. It said the captioner's cost was flat above a 768px rendition
// because its processor capped the pixel budget at 512*512 — true of the cap,
// and the wrong conclusion, because it made a virtue of downscaling 96% of the
// library back down again. That cap is 1024*1024 as of the bench in photo_ml's
// captioner.py, so these renditions are now read at something nearer their own
// size: 907 image tokens against 235, a describe pass that roughly doubles, and
// captions that stop calling a phone "an object" and a home screen "a clock".
//
// Changing this is a deliberate requeue — `photobackup ml renditions` — for the
// reason jobs.ReconcileMLPrep gives: the evidence this job ran is a file on
// disk rather than a row, so nothing notices a size change by itself.
const MLEdge = 1536

// MLFrameCount is how many frames are taken from a video.
//
// One rendition per still and several per clip, because a video is not one
// picture. A clip that starts on a beach and ends in a restaurant is findable
// as both only if both were looked at; averaging a whole clip into one
// description would make it neither.
const MLFrameCount = 6

// MLFrameSuffix names one sampled frame, numbered from zero.
func MLFrameSuffix(frame int) string {
	return fmt.Sprintf(".ml.%d.webp", frame)
}

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

// StageDir creates an empty directory in the staging area and returns it with a
// cleanup that removes it and everything under it.
//
// It exists for the one output that is a set of files rather than a file:
// ffmpeg writes a sampled sequence through an image2 pattern — frame-1.webp,
// frame-2.webp — and a pattern needs a directory to expand into. The
// alternative is one ffmpeg per frame, which is one decode of the clip per
// frame.
func (s *Store) StageDir(name string) (dir string, cleanup func(), err error) {
	parent := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create staging dir: %w", err)
	}
	dir, err = os.MkdirTemp(parent, name)
	if err != nil {
		return "", func() {}, fmt.Errorf("create staging directory: %w", err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
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

// Suffixes is every rendition this store can hold for one original: the stills
// at each size, the motion at each size, and the two video renditions.
//
// JoinPreview is deliberately not among them. It belongs to a merge group
// rather than to an asset, so every caller here — purging one photograph,
// sealing one into the vault, restoring one out of it — would be reaching for
// a file that is not theirs.
//
// Written as a function rather than a var because it is derived from
// ThumbSizes, and a list that had to be kept in step with that one by hand is a
// list that would be wrong the first time a size was added — which is exactly
// the moment a purge would start leaving files behind.
func Suffixes() []string {
	out := make([]string, 0, len(ThumbSizes)*2+MLFrameCount+3)
	for _, size := range ThumbSizes {
		out = append(out, ThumbSuffix(size), LiveSuffix(size))
	}
	// The ML renditions belong here for both of the reasons this list exists.
	// Purging a photograph has to take them — they are a legible copy of it, at
	// MLEdge, and leaving them behind would be leaving the picture behind. And
	// hiding one has to seal them: a rendition of a photograph in the vault
	// cannot stay readable on the SSD.
	out = append(out, MLStill)
	for frame := range MLFrameCount {
		out = append(out, MLFrameSuffix(frame))
	}
	return append(out, Playback, PlaybackPlain)
}

// RemoveAll deletes every rendition of an original, reporting how many files
// there were and the bytes they took.
//
// Missing files are the ordinary case rather than a failure: most assets have
// four of these and no asset has all fifteen.
func (s *Store) RemoveAll(sha256hex string) (files int, bytes int64, err error) {
	for _, suffix := range Suffixes() {
		path := s.Path(sha256hex, suffix)
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			// Reported, but the rest are still attempted: one undeletable
			// thumbnail should not strand the other seven files.
			err = fmt.Errorf("remove derivative %s: %w", path, rmErr)
			continue
		}
		files++
		bytes += info.Size()
	}
	return files, bytes, err
}

// Usage adds up every rendition on disk, grouped by whatever classify makes of
// the digest it came from. A digest classify has no opinion about — one whose
// asset has since been purged, or one in the vault — is returned under the
// empty string, so nothing on the disk goes uncounted.
//
// The walk is the only way to ask this. Derivatives are files rather than rows:
// nothing records their sizes, they are rebuilt whenever a job re-runs, and a
// count kept in the database would be wrong the first time somebody deleted the
// tree to reclaim the SSD — which is a supported thing to do, because every
// byte here can be regenerated from a blob.
//
// The vault's encrypted renditions sit under `vault/` and the staging files
// under `tmp/`; both are skipped. The first is not attributable to anything
// without the vault key, and the second is not a derivative yet.
func (s *Store) Usage(classify func(sha256hex string) string) (map[string]int64, error) {
	usage := make(map[string]int64)

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A tree that is not there yet is an archive with no thumbnails,
			// not a failure to report one.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if path != s.root && (d.Name() == "vault" || d.Name() == "tmp") {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		// Every name in this tree is a 64-character digest plus a suffix, by
		// construction — see Path. Anything else was put here by hand.
		name := d.Name()
		var sha string
		if len(name) > 64 {
			sha = name[:64]
		}
		usage[classify(sha)] += info.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("measure derivatives: %w", err)
	}
	return usage, nil
}

// Keys lists the digests this store holds a file of one suffix for.
//
// It exists for the one derivative nothing else can account for. Every other
// file here is named after an asset, so "is this still wanted" is answered by
// looking the asset up; a join preview is named after a merge group, and a
// group can stop wanting one in ways that never touch this package — its parts
// purged out from under it, its question answered on another machine. The
// sweep that reconciles the two needs the list.
func (s *Store) Keys(suffix string) ([]string, error) {
	var keys []string

	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if path != s.root && (d.Name() == "vault" || d.Name() == "tmp") {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if len(name) == 64+len(suffix) && strings.HasSuffix(name, suffix) {
			keys = append(keys, name[:64])
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s derivatives: %w", suffix, err)
	}
	return keys, nil
}
