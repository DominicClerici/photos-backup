package derive

import (
	"bytes"
	"context"
	"fmt"
	"os"
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

func decodeFile(t *testing.T, path string) (width, height int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendition: %v", err)
	}
	return decode(t, bytes.NewBuffer(data))
}

// targets names one file per size in a directory that goes away with the test.
func targets(t *testing.T, sizes ...int) []ThumbTarget {
	t.Helper()
	dir := t.TempDir()
	out := make([]ThumbTarget, len(sizes))
	for i, size := range sizes {
		out[i] = ThumbTarget{Size: size, Path: filepath.Join(dir, fmt.Sprint(size)+".webp")}
	}
	return out
}

func TestThumbsAreAlwaysSquareAtEverySize(t *testing.T) {
	// 400x300, so the crop has to take a slice off both sides rather than
	// letterboxing, and 512 is past the original — the small sizes must not
	// inherit anything from that.
	want := targets(t, 96, 256, 512)
	if err := New().Thumbs(context.Background(), fixture("sample.heic"), want); err != nil {
		t.Fatalf("Thumbs: %v\n\nIs ImageMagick installed with the libheif delegate?", err)
	}

	for _, target := range want {
		w, h := decodeFile(t, target.Path)
		if w != target.Size || h != target.Size {
			t.Errorf("thumb is %dx%d, want %dx%d", w, h, target.Size, target.Size)
		}
	}
}

// A portrait original must come out square too. If the crop were skipped for
// one aspect ratio, the grid's fixed row height would break.
func TestThumbsSquareARealIPhonePortrait(t *testing.T) {
	want := targets(t, 96, 256, 512)
	if err := New().Thumbs(context.Background(), fixture("iphone-portrait.heic"), want); err != nil {
		t.Fatalf("Thumbs: %v", err)
	}

	for _, target := range want {
		w, h := decodeFile(t, target.Path)
		if w != target.Size || h != target.Size {
			t.Errorf("thumb is %dx%d, want %dx%d", w, h, target.Size, target.Size)
		}
	}
}

// The chain steps down from the largest size, so the targets arriving in any
// order has to mean the same thing.
func TestThumbsDoNotDependOnTheOrderAskedFor(t *testing.T) {
	want := targets(t, 256, 512, 96)
	if err := New().Thumbs(context.Background(), fixture("iphone-portrait.heic"), want); err != nil {
		t.Fatalf("Thumbs: %v", err)
	}

	for _, target := range want {
		w, h := decodeFile(t, target.Path)
		if w != target.Size || h != target.Size {
			t.Errorf("thumb is %dx%d, want %dx%d", w, h, target.Size, target.Size)
		}
	}
}

func TestThumbsHonorAnyRequestedSize(t *testing.T) {
	want := targets(t, 64)
	if err := New().Thumbs(context.Background(), fixture("sample.heic"), want); err != nil {
		t.Fatalf("Thumbs: %v", err)
	}

	w, h := decodeFile(t, want[0].Path)
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

// A Google Takeout of iPhone ProRAW hands back re-encoded JPEGs that kept their
// .dng names. ImageMagick picks the RAW decoder off the extension, so trusting
// the name means libraw is asked to demosaic a JPEG and the asset parks as
// unreadable — with the photo perfectly intact on disk the whole time.
func TestThumbsRenderAJPEGThatKeptARawExtension(t *testing.T) {
	mislabeled := filepath.Join(t.TempDir(), "IMG_4471.dng")
	data, err := os.ReadFile(fixture("photo.jpg"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(mislabeled, data, 0o600); err != nil {
		t.Fatalf("write mislabeled fixture: %v", err)
	}

	want := targets(t, 96, 512)
	if err := New().Thumbs(context.Background(), mislabeled, want); err != nil {
		t.Fatalf("Thumbs on a JPEG named .dng: %v", err)
	}
	for _, target := range want {
		if w, h := decodeFile(t, target.Path); w != target.Size || h != target.Size {
			t.Errorf("thumb is %dx%d, want %dx%d", w, h, target.Size, target.Size)
		}
	}
}

func TestSniffNamesTheCoderTheBytesCallFor(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1}
	heic := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0}
	webp := []byte{'R', 'I', 'F', 'F', 8, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
	// A DNG is a TIFF. Sniffing it must stay silent: naming a coder here would
	// take every genuine RAW away from libraw, which is the only thing that can
	// demosaic one.
	dng := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 3, 0, 0, 1, 4, 0, 1, 0}

	for _, tc := range []struct {
		name  string
		head  []byte
		coder string
	}{
		{"jpeg", jpeg, "jpeg"},
		{"heic", heic, "heic"},
		{"webp", webp, "webp"},
		{"dng stays with libraw", dng, ""},
		{"too short to tell", jpeg[:4], ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "blob")
			if err := os.WriteFile(path, tc.head, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := sniff(path); got != tc.coder {
				t.Errorf("sniff = %q, want %q", got, tc.coder)
			}
		})
	}
}

func TestSourceKeepsTheFrameSelectorWithACoder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.dng")
	if err := os.WriteFile(path, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := source(path), "jpeg:"+path+"[0]"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}
	missing := filepath.Join(t.TempDir(), "gone.heic")
	if got, want := source(missing), missing+"[0]"; got != want {
		t.Errorf("source of an unreadable file = %q, want %q", got, want)
	}
}

func TestConversionReportsTheFailingInput(t *testing.T) {
	err := New().Thumbs(context.Background(), fixture("does-not-exist.heic"), targets(t, 256))
	if err == nil {
		t.Fatal("Thumbs succeeded on a missing file, want an error")
	}
	// The error text lands in jobs.last_error, so it has to identify the file.
	if !bytes.Contains([]byte(err.Error()), []byte("does-not-exist.heic")) {
		t.Errorf("error does not mention the failing input: %v", err)
	}
}
