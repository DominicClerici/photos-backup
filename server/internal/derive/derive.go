// Package derive renders web-viewable images from stored originals by driving
// ImageMagick, which reads HEIC through libheif. Go only builds argv.
//
// Two renditions, and they are stored differently on purpose:
//
//   - Thumb is 256px square and written to disk once, because the grid asks for
//     hundreds at a time and converting on demand would mean hundreds of
//     ImageMagick processes per scroll.
//   - Preview is 2048px and converted per request, never stored. One viewer
//     shows one preview at a time, and the bytes are content-addressed, so the
//     browser's own cache does the caching that a derivative file would.
package derive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

type Converter struct {
	// Binary is the ImageMagick entry point. Overridable for tests and for
	// hosts where it is not on PATH.
	Binary string

	ThumbSize    int
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
		ThumbSize:          256,
		ThumbQuality:       75,
		PreviewSize:        2048,
		PreviewQuality:     82,
		PreviewConcurrency: 4,
	}
}

// Thumb writes a square, center-cropped WebP of the source.
//
// Square because the grid is fixed square cells: cropping here means the
// browser never downscales, and the timeline can compute its layout from the
// row height alone without knowing any image's aspect ratio. The cost is that
// the grid crops — the same trade Photos makes.
func (c *Converter) Thumb(ctx context.Context, src string, w io.Writer) error {
	size := fmt.Sprintf("%dx%d", c.thumbSize(), c.thumbSize())
	return c.run(ctx, w, src,
		"-auto-orient",
		// ^ fills the box, then extent crops the overflow away.
		"-resize", size+"^",
		"-gravity", "center",
		"-extent", size,
		"-quality", fmt.Sprint(c.thumbQuality()),
	)
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
	return c.run(ctx, w, src,
		"-auto-orient",
		"-resize", resize,
		"-quality", fmt.Sprint(c.previewQuality()),
	)
}

// run drives one conversion to WebP on stdout.
func (c *Converter) run(ctx context.Context, w io.Writer, src string, ops ...string) error {
	// [0] takes the primary image. A HEIC can hold several — a burst, or the
	// stills of a Live Photo — and without this ImageMagick renders every one.
	args := append([]string{src + "[0]"}, ops...)
	// Drop EXIF, IPTC, and XMP but keep the colour profile. A blanket -strip
	// would take the ICC profile with it, and iPhone photos are Display P3:
	// stripped and reinterpreted as sRGB, every rendition comes out visibly
	// desaturated.
	args = append(args, "+profile", "!icc,icm", "webp:-")

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

func (c *Converter) thumbSize() int {
	if c.ThumbSize <= 0 {
		return 256
	}
	return c.ThumbSize
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
