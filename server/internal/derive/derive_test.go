package derive

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
	if err := New().Thumbs(context.Background(), fixture("sample.heic"), nil, want); err != nil {
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
	if err := New().Thumbs(context.Background(), fixture("iphone-portrait.heic"), nil, want); err != nil {
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
	if err := New().Thumbs(context.Background(), fixture("iphone-portrait.heic"), nil, want); err != nil {
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
	if err := New().Thumbs(context.Background(), fixture("sample.heic"), nil, want); err != nil {
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
	if err := c.Preview(context.Background(), fixture("sample.heic"), nil, 0, &out); err != nil {
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
	if err := New().Preview(context.Background(), fixture("sample.heic"), nil, 0, &out); err != nil {
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
	if err := c.Preview(context.Background(), fixture("iphone-portrait.heic"), nil, 0, &out); err != nil {
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
			if err := c.Preview(context.Background(), fixture("sample.heic"), nil, 0, &out); err != nil {
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
	if err := New().Thumbs(context.Background(), mislabeled, nil, want); err != nil {
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
	err := New().Thumbs(context.Background(), fixture("does-not-exist.heic"), nil, targets(t, 256))
	if err == nil {
		t.Fatal("Thumbs succeeded on a missing file, want an error")
	}
	// The error text lands in jobs.last_error, so it has to identify the file.
	if !bytes.Contains([]byte(err.Error()), []byte("does-not-exist.heic")) {
		t.Errorf("error does not mention the failing input: %v", err)
	}
}

// layerFixture writes a PNG that is opaque red over its left half and fully
// transparent over its right, deliberately at a size and aspect ratio that
// match no fixture: a Snapchat caption layer is the phone's screen and the
// photograph is what the camera captured to fill it, and across this archive
// the two never once agree.
func layerFixture(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width / 2 {
			img.Set(x, y, color.NRGBA{R: 255, A: 255})
		}
	}

	path := filepath.Join(t.TempDir(), "overlay.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create layer: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode layer: %v", err)
	}
	return path
}

// at reads one pixel out of a rendition, as 8-bit RGB.
func at(t *testing.T, out *bytes.Buffer, x, y int) (r, g, b uint8) {
	t.Helper()
	img, err := webp.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("output is not decodable WebP: %v", err)
	}
	cr, cg, cb, _ := img.At(x, y).RGBA()
	return uint8(cr >> 8), uint8(cg >> 8), uint8(cb >> 8)
}

// The layer is stretched to the photograph's frame rather than placed at its
// own size, so a caption written across the middle of a phone screen lands
// across the middle of the picture whatever resolution either file happens to
// be. Half the layer is transparent, which is the half that proves it is being
// composed rather than drawn over.
func TestPreviewComposesALayerOverThePhotograph(t *testing.T) {
	c := New()
	// The fixture is 400x300; the layer is a different size and a different
	// shape, which is the whole point.
	layer := &Layer{Path: layerFixture(t, 101, 253), Width: 400, Height: 300}

	var plain bytes.Buffer
	if err := c.Preview(context.Background(), fixture("sample.heic"), nil, 0, &plain); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	var composed bytes.Buffer
	if err := c.Preview(context.Background(), fixture("sample.heic"), layer, 0, &composed); err != nil {
		t.Fatalf("Preview with a layer: %v", err)
	}

	if w, h := decode(t, &composed); w != 400 || h != 300 {
		t.Errorf("composite is %dx%d, want the photograph's own 400x300", w, h)
	}

	if r, g, b := at(t, &composed, 100, 150); r < 200 || g > 60 || b > 60 {
		t.Errorf("the layer's own half reads rgb(%d,%d,%d), want it drawn in red", r, g, b)
	}

	// And the transparent half is untouched, to the tolerance two lossy
	// encodes of the same pixels can be expected to agree to.
	wantR, wantG, wantB := at(t, &plain, 300, 150)
	gotR, gotG, gotB := at(t, &composed, 300, 150)
	for _, d := range []struct {
		name     string
		got, was uint8
	}{{"red", gotR, wantR}, {"green", gotG, wantG}, {"blue", gotB, wantB}} {
		if diff := int(d.got) - int(d.was); diff > 8 || diff < -8 {
			t.Errorf("the layer's transparent half changed the %s channel: %d, was %d",
				d.name, d.got, d.was)
		}
	}
}

// A caller that has not read the photograph yet — a preview requested while the
// metadata job that records width and height is still queued — leaves the size
// at zero and gets the same picture, one `magick identify` later.
func TestPreviewMeasuresThePhotographWhenTheLayerDoesNotSayHowBig(t *testing.T) {
	layer := &Layer{Path: layerFixture(t, 101, 253)}

	var out bytes.Buffer
	if err := New().Preview(context.Background(), fixture("sample.heic"), layer, 0, &out); err != nil {
		t.Fatalf("Preview with an unmeasured layer: %v", err)
	}
	if w, h := decode(t, &out); w != 400 || h != 300 {
		t.Errorf("composite is %dx%d, want the photograph's own 400x300", w, h)
	}
	if r, g, b := at(t, &out, 100, 150); r < 200 || g > 60 || b > 60 {
		t.Errorf("the layer's own half reads rgb(%d,%d,%d), want it drawn in red", r, g, b)
	}
}

// The layer goes on before the square crop, so the grid's tile shows the same
// picture the viewer does rather than a caption in a different place.
func TestThumbsComposeTheLayerBeforeCropping(t *testing.T) {
	layer := &Layer{Path: layerFixture(t, 101, 253), Width: 400, Height: 300}

	want := targets(t, 256)
	if err := New().Thumbs(context.Background(), fixture("sample.heic"), layer, want); err != nil {
		t.Fatalf("Thumbs with a layer: %v", err)
	}
	if w, h := decodeFile(t, want[0].Path); w != 256 || h != 256 {
		t.Errorf("thumb is %dx%d, want 256x256", w, h)
	}

	data, err := os.ReadFile(want[0].Path)
	if err != nil {
		t.Fatalf("read thumb: %v", err)
	}
	buf := bytes.NewBuffer(data)
	// A 400x300 photograph cropped square keeps its middle 300 columns, so the
	// layer's red half covers the left 50 of the 256 that survive.
	if r, g, b := at(t, buf, 20, 128); r < 200 || g > 60 || b > 60 {
		t.Errorf("the cropped thumb reads rgb(%d,%d,%d) where the layer is, want red", r, g, b)
	}
}

// A layer naming a file that is not there must fail loudly rather than
// silently rendering the photograph alone: a memory quietly missing its caption
// looks exactly like a memory that never had one.
func TestConversionReportsAMissingLayer(t *testing.T) {
	layer := &Layer{Path: filepath.Join(t.TempDir(), "gone.png"), Width: 400, Height: 300}

	var out bytes.Buffer
	err := New().Preview(context.Background(), fixture("sample.heic"), layer, 0, &out)
	if err == nil {
		t.Fatal("Preview succeeded with a missing layer, want an error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("gone.png")) {
		t.Errorf("error does not mention the missing layer: %v", err)
	}
}

// A source that is already inside the preview box gets no downscale, and a
// downscale is what usually hides the renderer's own compression. Told the
// source's size, Preview stops spending that hiding place it does not have.
//
// The fixture is 400x300, so a PreviewSize of 2048 resizes nothing either way;
// the only difference between the two renders is the quality that was chosen.
func TestPreviewSpendsMoreOnASourceItWillNotDownscale(t *testing.T) {
	var blind, told bytes.Buffer

	if err := New().Preview(context.Background(), fixture("sample.heic"), nil, 0, &blind); err != nil {
		t.Fatalf("Preview without a size: %v", err)
	}
	if err := New().Preview(context.Background(), fixture("sample.heic"), nil, 400, &told); err != nil {
		t.Fatalf("Preview with a size: %v", err)
	}

	if told.Len() <= blind.Len() {
		t.Errorf("preview of a small source is %d bytes, no larger than the %d spent when the size was unknown",
			told.Len(), blind.Len())
	}

	w, h := decode(t, &told)
	if w != 400 || h != 300 {
		t.Errorf("preview is %dx%d, want the original 400x300", w, h)
	}
}

// The other half of the same rule: a camera original is downscaled by three
// quarters, which is where the modest quality belongs and where the extra bytes
// would buy nothing an eye could find.
func TestPreviewDoesNotSpendMoreOnASourceItWillDownscale(t *testing.T) {
	c := New()
	c.PreviewSize = 200

	var out bytes.Buffer
	if err := c.Preview(context.Background(), fixture("sample.heic"), nil, 400, &out); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	var same bytes.Buffer
	if err := c.Preview(context.Background(), fixture("sample.heic"), nil, 0, &same); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	if out.Len() != same.Len() {
		t.Errorf("downscaled preview is %d bytes told the source size and %d bytes not; they should be the same render",
			out.Len(), same.Len())
	}
}
