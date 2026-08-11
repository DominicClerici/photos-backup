// Package derive renders web-viewable images from stored originals by driving
// ImageMagick, which reads HEIC through libheif. Go only builds argv.
//
// Two renditions, and they are stored differently on purpose:
//
//   - Thumbs are the square sizes the grid draws, written to disk once, because
//     the grid asks for hundreds at a time and converting on demand would mean
//     hundreds of ImageMagick processes per scroll.
//   - Preview is 2048px and converted per request, never stored. One viewer
//     shows one preview at a time, and the bytes are content-addressed, so the
//     browser's own cache does the caching that a derivative file would.
package derive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
)

type Converter struct {
	// Binary is the ImageMagick entry point. Overridable for tests and for
	// hosts where it is not on PATH.
	Binary string

	ThumbQuality int

	// PreviewSize bounds the longest edge. Images already smaller are not
	// scaled up.
	PreviewSize    int
	PreviewQuality int
	// PreviewConcurrency caps simultaneous on-demand conversions. Without it, a
	// viewer opened while prefetching neighbours can fork an ImageMagick per
	// in-flight request and take the machine down; requests past the cap wait
	// rather than pile on.
	PreviewConcurrency int

	once     sync.Once
	previews chan struct{}
}

func New() *Converter {
	return &Converter{
		Binary:             "magick",
		ThumbQuality:       75,
		PreviewSize:        2048,
		PreviewQuality:     82,
		PreviewConcurrency: 4,
	}
}

// ThumbTarget is one size of a still and the file it should be written to.
type ThumbTarget struct {
	Size int
	Path string
}

// keepColour drops EXIF, IPTC, and XMP but keeps the colour profile. A blanket
// -strip would take the ICC profile with it, and iPhone photos are Display P3:
// stripped and reinterpreted as sRGB, every rendition comes out visibly
// desaturated.
//
// It goes after -auto-orient, which reads the orientation this drops, and
// before any output — a chain that writes several files has no single end to
// hang it on.
var keepColour = []string{"+profile", "!icc,icm"}

// Thumbs writes a square, center-cropped WebP of the source at every size asked
// for, in one ImageMagick process.
//
// Square because the grid is fixed square cells: cropping here means the
// browser never downscales, and the timeline can compute its layout from the
// row height alone without knowing any image's aspect ratio. The cost is that
// the grid crops — the same trade Photos makes.
//
// One process rather than one per size because decoding is what this costs. A
// 12-megapixel HEIC takes a few hundred milliseconds to get into memory and a
// couple of milliseconds to squeeze into a 96px square, so a run per size would
// roughly triple the job to produce a third more bytes. The chain renders the
// largest size first and steps down through it, which is also why the small
// sizes come out of an already-cropped square rather than repeating the crop.
func (c *Converter) Thumbs(ctx context.Context, src string, targets []ThumbTarget) error {
	if len(targets) == 0 {
		return nil
	}
	ordered := slices.SortedFunc(slices.Values(targets), func(a, b ThumbTarget) int {
		return b.Size - a.Size
	})

	args := []string{"-auto-orient"}
	args = append(args, keepColour...)
	args = append(args,
		"-quality", fmt.Sprint(c.thumbQuality()),
		"-gravity", "center",
	)
	for _, t := range ordered {
		box := fmt.Sprintf("%dx%d", t.Size, t.Size)
		// ^ fills the box, then extent crops the overflow away.
		args = append(args, "-resize", box+"^", "-extent", box, "-write", "webp:"+t.Path)
	}
	// Every size is written by an explicit -write, so the pipeline ends with
	// nothing left to encode. null: says so, rather than leaving the last
	// rendition to be named twice.
	args = append(args, "null:")

	if err := c.run(ctx, nil, src, args...); err != nil {
		return err
	}
	// ImageMagick can exit 0 having written a file it could not finish — a full
	// disk is the way that happens here. Committing an empty thumbnail would
	// hand the gallery a broken tile with a job marked done behind it, so the
	// files are checked before the caller is told they exist.
	for _, t := range ordered {
		info, err := os.Stat(t.Path)
		if err != nil {
			return fmt.Errorf("convert %s to a %dpx thumbnail: %w", src, t.Size, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("convert %s to a %dpx thumbnail: nothing was written", src, t.Size)
		}
	}
	return nil
}

// Preview writes a WebP bounded by PreviewSize on its longest edge.
func (c *Converter) Preview(ctx context.Context, src string, w io.Writer) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	// The trailing > means "shrink only": a photo smaller than the box is left
	// alone rather than blown up into a blurry larger file.
	resize := fmt.Sprintf("%dx%d>", c.previewSize(), c.previewSize())
	ops := []string{"-auto-orient"}
	ops = append(ops, keepColour...)
	ops = append(ops,
		"-resize", resize,
		"-quality", fmt.Sprint(c.previewQuality()),
		"webp:-",
	)
	return c.run(ctx, w, src, ops...)
}

// run drives one ImageMagick, with ops carrying its own output specification —
// stdout for the preview, a file per size for the thumbnails.
func (c *Converter) run(ctx context.Context, w io.Writer, src string, ops ...string) error {
	// [0] takes the primary image. A HEIC can hold several — a burst, or the
	// stills of a Live Photo — and without this ImageMagick renders every one.
	args := append([]string{src + "[0]"}, ops...)

	cmd := exec.CommandContext(ctx, c.binary(), args...)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("convert %s to webp: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (c *Converter) acquire(ctx context.Context) error {
	c.once.Do(func() {
		n := c.PreviewConcurrency
		if n <= 0 {
			n = 4
		}
		c.previews = make(chan struct{}, n)
	})

	select {
	case c.previews <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Converter) release() { <-c.previews }

func (c *Converter) binary() string {
	if c.Binary == "" {
		return "magick"
	}
	return c.Binary
}

func (c *Converter) thumbQuality() int {
	if c.ThumbQuality <= 0 {
		return 75
	}
	return c.ThumbQuality
}

func (c *Converter) previewSize() int {
	if c.PreviewSize <= 0 {
		return 2048
	}
	return c.PreviewSize
}

func (c *Converter) previewQuality() int {
	if c.PreviewQuality <= 0 {
		return 82
	}
	return c.PreviewQuality
}
