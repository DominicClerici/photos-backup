// Package derive renders web-viewable images from stored originals.
//
// Phase 1 converts on every request and caches nothing. Decoding is delegated
// to ImageMagick, which reads HEIC through libheif; Go only orchestrates.
package derive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

type Converter struct {
	// Binary is the ImageMagick entry point. Overridable for tests and for
	// hosts where it is not on PATH.
	Binary string
	// MaxDimension bounds the longest edge. Images smaller than this are not
	// scaled up.
	MaxDimension int
	Quality      int
}

func New() *Converter {
	return &Converter{Binary: "magick", MaxDimension: 1600, Quality: 82}
}

// ToWebP decodes src and writes a WebP rendition to w.
func (c *Converter) ToWebP(ctx context.Context, src string, w io.Writer) error {
	resize := fmt.Sprintf("%dx%d>", c.MaxDimension, c.MaxDimension)
	cmd := exec.CommandContext(ctx, c.Binary,
		src,
		"-auto-orient",
		"-resize", resize,
		"-quality", fmt.Sprint(c.Quality),
		"webp:-",
	)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("convert %s to webp: %w: %s", src, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
