package mediatype_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
)

// head reads what Sniff would be given for a real fixture.
func head(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	buf := make([]byte, mediatype.SniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("read fixture: %v", err)
	}
	return buf[:n]
}

func TestSniffRealFixtures(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"iphone-portrait.heic", "image/heic"},
		{"sample.heic", "image/heic"},
		{"photo.jpg", "image/jpeg"},
		{"bare.jpg", "image/jpeg"},
		{"clip.mov", "video/quicktime"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			if got := mediatype.Sniff(head(t, tc.file)); got != tc.want {
				t.Errorf("Sniff(%s) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

// A Takeout export strips the extension off Live Photo videos. Before sniffing,
// 216 of the 3,116 files in the test corpus arrived this way and were filed as
// images that could never produce a thumbnail.
func TestDetectExtensionlessQuickTime(t *testing.T) {
	ct, ext := mediatype.Detect("IMG_7266", head(t, "clip.mov"))

	if ct != "video/quicktime" {
		t.Errorf("content type = %q, want video/quicktime", ct)
	}
	if ext != ".mov" {
		t.Errorf("ext = %q, want .mov", ext)
	}
	if got := mediatype.Kind(ct); got != mediatype.KindVideo {
		t.Errorf("kind = %q, want video", got)
	}
}

// A recognised extension is trusted without reading a byte, so an upload whose
// body has not arrived yet still classifies.
func TestDetectPrefersKnownExtension(t *testing.T) {
	ct, ext := mediatype.Detect("IMG_0001.HEIC", nil)

	if ct != "image/heic" {
		t.Errorf("content type = %q, want image/heic", ct)
	}
	if ext != ".heic" {
		t.Errorf("ext = %q, want .heic", ext)
	}
}

func TestDetectUnclassifiable(t *testing.T) {
	ct, ext := mediatype.Detect("notes.txt", []byte("just some text"))

	if ct != mediatype.Octet {
		t.Errorf("content type = %q, want %q", ct, mediatype.Octet)
	}
	// The name's extension survives, because throwing it away would lose the
	// only thing known about the file.
	if ext != ".txt" {
		t.Errorf("ext = %q, want .txt", ext)
	}
	if got := mediatype.Kind(ct); got != mediatype.KindImage {
		t.Errorf("kind = %q, want image", got)
	}
}

func TestSniffSignatures(t *testing.T) {
	riff := func(form string) []byte {
		b := append([]byte("RIFF"), 0, 0, 0, 0)
		return append(b, []byte(form)...)
	}
	ftyp := func(brands ...string) []byte {
		b := []byte{0, 0, 0, 0, 'f', 't', 'y', 'p'}
		for _, brand := range brands {
			b = append(b, []byte(brand)...)
		}
		b[3] = byte(len(b))
		return b
	}

	for _, tc := range []struct {
		name string
		in   []byte
		want string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}, "image/png"},
		{"gif", []byte("GIF89a and then some"), "image/gif"},
		{"webp", riff("WEBP"), "image/webp"},
		{"avi", riff("AVI "), "video/x-msvideo"},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0}, "video/webm"},
		{"tiff little endian", []byte("II\x2A\x00rest"), "image/tiff"},
		{"tiff big endian", []byte("MM\x00\x2Arest"), "image/tiff"},
		{"quicktime", ftyp("qt  ", "\x00\x00\x02\x00", "qt  "), "video/quicktime"},
		{"mp4", ftyp("isom", "\x00\x00\x02\x00", "isom", "mp41"), "video/mp4"},
		// The major brand names no format anyone sniffs for; "heic" only shows
		// up in the compatible brands, past the version number.
		{"heic behind mif1", ftyp("mif1", "\x00\x00\x00\x00", "mif1", "heic"), "image/heic"},
		{"empty", nil, ""},
		{"too short to be ftyp", []byte("\x00\x00\x00\x18ftyp"), ""},
		{"unknown", []byte("nothing recognisable here at all"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mediatype.Sniff(tc.in); got != tc.want {
				t.Errorf("Sniff() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A truncated box length must not send the brand walk past the end of the
// buffer. The header claims 4KB; only 20 bytes were read.
func TestSniffToleratesOverlongBoxSize(t *testing.T) {
	b := []byte{0x00, 0x00, 0x10, 0x00, 'f', 't', 'y', 'p'}
	b = append(b, []byte("qt  ")...)
	b = append(b, []byte("\x00\x00\x02\x00")...)

	if got := mediatype.Sniff(b); got != "video/quicktime" {
		t.Errorf("Sniff() = %q, want video/quicktime", got)
	}
}

func TestExtForAndFromExt(t *testing.T) {
	if got := mediatype.FromExt("HEIC"); got != "image/heic" {
		t.Errorf("FromExt(HEIC) = %q, want image/heic", got)
	}
	if got := mediatype.FromExt(".unknown"); got != "" {
		t.Errorf("FromExt(.unknown) = %q, want empty", got)
	}
	if got := mediatype.ExtFor("video/quicktime"); got != ".mov" {
		t.Errorf("ExtFor(video/quicktime) = %q, want .mov", got)
	}
	if got := mediatype.ExtFor(mediatype.Octet); got != "" {
		t.Errorf("ExtFor(octet-stream) = %q, want empty", got)
	}
}
