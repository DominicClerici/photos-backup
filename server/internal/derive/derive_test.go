package derive

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/image/webp"
)

// Fixtures are shared across the media packages, since they are binary and the
// same HEIC serves derive, exifdata, and the worker.
func fixture(name string) string { return filepath.Join("..", "..", "testdata", name) }

func decode(t *testing.T, out *bytes.Buffer) (width, height int) {
	t.Helper()
	if out.Len() == 0 {
		t.Fatal("conversion produced no bytes")
	}
	img, err := webp.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("output is not decodable WebP: %v", err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

func TestThumbIsAlwaysSquare(t *testing.T) {
	var out bytes.Buffer
	// 400x300, so the crop has to take a slice off both sides rather than
	// letterboxing.
	if err := New().Thumb(context.Background(), fixture("sample.heic"), &out); err != nil {
		t.Fatalf("Thumb: %v\n\nIs ImageMagick installed with the libheif delegate?", err)
	}

	w, h := decode(t, &out)
	if w != 256 || h != 256 {
		t.Errorf("thumb is %dx%d, want 256x256", w, h)
	}
}

// A portrait original must come out square too. If the crop were skipped for
// one aspect ratio, the grid's fixed row height would break.
func TestThumbSquaresARealIPhonePortrait(t *testing.T) {
	var out bytes.Buffer
	if err := New().Thumb(context.Background(), fixture("iphone-portrait.heic"), &out); err != nil {
		t.Fatalf("Thumb: %v", err)
	}

	w, h := decode(t, &out)
	if w != 256 || h != 256 {
		t.Errorf("thumb is %dx%d, want 256x256", w, h)
	}
}

func TestThumbHonorsAConfiguredSize(t *testing.T) {
	c := New()
	c.ThumbSize = 64

	var out bytes.Buffer
	if err := c.Thumb(context.Background(), fixture("sample.heic"), &out); err != nil {
		t.Fatalf("Thumb: %v", err)
	}

	w, h := decode(t, &out)
	if w != 64 || h != 64 {
		t.Errorf("thumb is %dx%d, want 64x64", w, h)
	}
}

func TestPreviewPreservesAspectRatio(t *testing.T) {
	c := New()
	c.PreviewSize = 100

	var out bytes.Buffer
	if err := c.Preview(context.Background(), fixture("sample.heic"), &out); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	w, h := decode(t, &out)
	if w != 100 || h != 75 {
		t.Errorf("preview is %dx%d, want 100x75", w, h)
	}
}

// The trailing ">" in the resize geometry is what stops a small original being
// blown up into a larger, blurrier file than the source.
func TestPreviewDoesNotUpscaleASmallOriginal(t *testing.T) {
	var out bytes.Buffer
	if err := New().Preview(context.Background(), fixture("sample.heic"), &out); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	w, h := decode(t, &out)
	if w != 400 || h != 300 {
		t.Errorf("preview is %dx%d, want the original 400x300", w, h)
	}
}

// -auto-orient has to be applied, or every portrait photo shows up on its side.
// The fixture's sensor read is 4032x3024 with "Rotate 90 CW".
func TestPreviewRotatesAccordingToOrientation(t *testing.T) {
	c := New()
	c.PreviewSize = 800

	var out bytes.Buffer
	if err := c.Preview(context.Background(), fixture("iphone-portrait.heic"), &out); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	w, h := decode(t, &out)
	if h <= w {
		t.Errorf("preview is %dx%d; the original is portrait once rotated", w, h)
	}
	if w != 600 || h != 800 {
		t.Errorf("preview is %dx%d, want 600x800", w, h)
	}
}

func TestPreviewConcurrencyIsCapped(t *testing.T) {
	c := New()
	c.PreviewConcurrency = 2

	// Fill the semaphore, then confirm a third caller is refused once its
	// context is cancelled rather than running anyway.
	ctx := context.Background()
	if err := c.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := c.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.acquire(cancelled); err == nil {
		t.Error("acquire exceeded PreviewConcurrency instead of waiting")
		c.release()
	}

	c.release()
	c.release()
}

func TestPreviewRunsConcurrentlyUpToTheCap(t *testing.T) {
	c := New()
	c.PreviewSize = 200

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if err := c.Preview(context.Background(), fixture("sample.heic"), &out); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Preview: %v", err)
	}
}

func TestConversionReportsTheFailingInput(t *testing.T) {
	var out bytes.Buffer

	err := New().Thumb(context.Background(), fixture("does-not-exist.heic"), &out)
	if err == nil {
		t.Fatal("Thumb succeeded on a missing file, want an error")
	}
	// The error text lands in jobs.last_error, so it has to identify the file.
	if !bytes.Contains([]byte(err.Error()), []byte("does-not-exist.heic")) {
		t.Errorf("error does not mention the failing input: %v", err)
	}
}
