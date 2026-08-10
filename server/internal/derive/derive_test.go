package derive

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/image/webp"
)

func TestToWebPConvertsHEIC(t *testing.T) {
	c := New()
	var out bytes.Buffer

	err := c.ToWebP(context.Background(), filepath.Join("testdata", "sample.heic"), &out)
	if err != nil {
		t.Fatalf("ToWebP: %v", err)
	}

	if out.Len() == 0 {
		t.Fatal("ToWebP produced no bytes")
	}
	img, err := webp.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("output is not decodable WebP: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 400 || b.Dy() != 300 {
		t.Errorf("decoded image is %dx%d, want 400x300", b.Dx(), b.Dy())
	}
}

func TestToWebPCapsLargeImagesToMaxDimension(t *testing.T) {
	c := New()
	c.MaxDimension = 100
	var out bytes.Buffer

	if err := c.ToWebP(context.Background(), filepath.Join("testdata", "sample.heic"), &out); err != nil {
		t.Fatalf("ToWebP: %v", err)
	}

	img, err := webp.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("output is not decodable WebP: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 100 || b.Dy() != 75 {
		t.Errorf("decoded image is %dx%d, want 100x75 (aspect preserved)", b.Dx(), b.Dy())
	}
}

// A real iPhone original: 3024x4032, ~1MB, HEVC Main Still Picture.
//
// This fixture's EXIF orientation is TopLeft, so it does NOT exercise
// -auto-orient — output is byte-identical with and without the flag. Verifying
// that needs an original shot with the phone held sideways; until such a
// fixture exists, rotation handling is untested.
func TestToWebPCapsRealIPhoneOriginal(t *testing.T) {
	c := New()
	var out bytes.Buffer

	err := c.ToWebP(context.Background(), filepath.Join("testdata", "iphone-portrait.heic"), &out)
	if err != nil {
		t.Fatalf("ToWebP: %v", err)
	}

	img, err := webp.Decode(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("output is not decodable WebP: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 1200 || b.Dy() != 1600 {
		t.Errorf("decoded image is %dx%d, want 1200x1600", b.Dx(), b.Dy())
	}
}

func TestToWebPReportsConverterStderrOnFailure(t *testing.T) {
	c := New()
	var out bytes.Buffer

	err := c.ToWebP(context.Background(), filepath.Join("testdata", "does-not-exist.heic"), &out)
	if err == nil {
		t.Fatal("ToWebP succeeded on a missing file, want an error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("does-not-exist.heic")) {
		t.Errorf("error does not mention the failing input, got: %v", err)
	}
}
