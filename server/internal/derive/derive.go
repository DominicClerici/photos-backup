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

// Layer is a second image drawn over the source before anything is rendered
// from it. Nil everywhere except a Snapchat memory, where the photograph and
// the handwriting over it are two archived files and the picture anybody
// actually saw is the two composed.
//
// Width and Height are the base's own dimensions, after orientation. The layer
// is stretched to exactly them rather than fitted, because the two files are
// the same frame photographed at different resolutions: across this archive's
// 439 memories the aspect ratios differ by at most 2%, and the overlay is
// registered to the frame edge, so stretching lands the caption where the
// person put it while fitting would leave it drifting a few pixels off. They
// are optional — a zero pair costs one `magick identify` to recover, which is
// the price of a caller that has not read the file yet.
type Layer struct {
	Path   string
	Width  int
	Height int
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
func (c *Converter) Thumbs(ctx context.Context, src string, over *Layer, targets []ThumbTarget) error {
	if len(targets) == 0 {
		return nil
	}
	ordered := slices.SortedFunc(slices.Values(targets), func(a, b ThumbTarget) int {
		return b.Size - a.Size
	})

	// The layer goes on at full resolution, before the square crop rather than
	// after: the crop then takes the same bite out of both, so a caption near
	// the edge is cut exactly where it would be cut in the picture.
	compose, err := c.compose(ctx, src, over)
	if err != nil {
		return err
	}

	args := []string{"-auto-orient"}
	args = append(args, compose...)
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
func (c *Converter) Preview(ctx context.Context, src string, over *Layer, w io.Writer) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	compose, err := c.compose(ctx, src, over)
	if err != nil {
		return err
	}

	// The trailing > means "shrink only": a photo smaller than the box is left
	// alone rather than blown up into a blurry larger file.
	resize := fmt.Sprintf("%dx%d>", c.previewSize(), c.previewSize())
	ops := []string{"-auto-orient"}
	ops = append(ops, compose...)
	ops = append(ops, keepColour...)
	ops = append(ops,
		"-resize", resize,
		"-quality", fmt.Sprint(c.previewQuality()),
		"webp:-",
	)
	return c.run(ctx, w, src, ops...)
}

// compose renders a layer as the ImageMagick operators that draw it over the
// image already in hand, or nothing at all when there is no layer.
//
// The parentheses are literal argv words, not shell syntax: they open a
// sub-sequence so the -resize inside applies to the layer alone and leaves the
// photograph underneath at full resolution. Everything after -composite sees a
// single image again, which is why the thumbnail and preview pipelines need no
// other change to draw a memory rather than a photograph.
func (c *Converter) compose(ctx context.Context, src string, over *Layer) ([]string, error) {
	if over == nil || over.Path == "" {
		return nil, nil
	}

	width, height := over.Width, over.Height
	if width <= 0 || height <= 0 {
		var err error
		if width, height, err = c.dimensions(ctx, src); err != nil {
			return nil, err
		}
	}

	return []string{
		"(", source(over.Path), "-auto-orient",
		"-resize", fmt.Sprintf("%dx%d!", width, height), ")",
		"-compose", "over", "-composite",
	}, nil
}

// dimensions asks ImageMagick how big an image is once its orientation has been
// applied, which is the size a layer has to be stretched to.
//
// A second process, and the reason Layer carries the numbers: every caller in
// the worker has just read them out of the file anyway, and this is here for
// the one that has not — a preview requested while the metadata job that fills
// in width and height is still queued.
func (c *Converter) dimensions(ctx context.Context, src string) (width, height int, err error) {
	cmd := exec.CommandContext(ctx, c.binary(), source(src), "-auto-orient", "-format", "%w %h", "info:")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, 0, fmt.Errorf("measure %s: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}
	if _, err := fmt.Sscan(stdout.String(), &width, &height); err != nil {
		return 0, 0, fmt.Errorf("measure %s: %q is not a size", src, stdout.String())
	}
	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("measure %s: reported %dx%d", src, width, height)
	}
	return width, height, nil
}

// run drives one ImageMagick, with ops carrying its own output specification —
// stdout for the preview, a file per size for the thumbnails.
func (c *Converter) run(ctx context.Context, w io.Writer, src string, ops ...string) error {
	args := append([]string{source(src)}, ops...)

	cmd := exec.CommandContext(ctx, c.binary(), args...)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("convert %s to webp: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// source names one file in ImageMagick's argv.
//
// [0] takes the primary image. A HEIC can hold several — a burst, or the
// stills of a Live Photo — and without this ImageMagick renders every one.
func source(path string) string {
	spec := path + "[0]"
	if coder := sniff(path); coder != "" {
		return coder + ":" + spec
	}
	return spec
}

// sniff names the ImageMagick coder that should read a file, from its leading
// bytes, or returns "" to leave the choice to ImageMagick.
//
// It exists because ImageMagick routes to libraw on the extension alone, and an
// export can hand us a file whose name has outlived its contents: a Google
// Takeout of iPhone ProRAW arrives as ordinary JPEGs still called .dng, and
// every one of them fails as "Unsupported file format or not RAW file" — a
// decoder error for a photo with nothing wrong with it. Sniffing makes the
// extension a hint and the bytes the fact, which is the same order of trust
// applyContentID already uses for pairing.
//
// The formats here are the ones whose magic is unambiguous. TIFF is absent on
// purpose: a DNG *is* a TIFF, so sniffing that would route every genuine RAW
// away from libraw and into a coder that cannot demosaic it — turning this from
// a fix into the same bug pointed the other way.
func sniff(path string) string {
	f, err := os.Open(path)
	if err != nil {
		// Nothing to report here: ImageMagick is about to fail on the same file
		// and its error says more than this one could.
		return ""
	}
	defer f.Close()

	var head [16]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && n < len(head) {
		return ""
	}
	b := head[:n]

	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif"
	case bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	}
	// ISO base media: the brand at byte 8 separates a still HEIF from an AVIF,
	// and both from the .mov that shares the container.
	if bytes.Equal(b[4:8], []byte("ftyp")) {
		switch string(b[8:12]) {
		case "avif", "avis":
			return "avif"
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
			return "heic"
		}
	}
	return ""
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
