package api

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"golang.org/x/image/webp"
)

func TestThumbServesTheStoredRendition(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	h.derive(t, up.ID)

	resp := h.get(t, "/v1/assets/"+up.ID+"/thumb")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/webp" {
		t.Errorf("Content-Type = %q, want image/webp", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	img, err := webp.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("thumb is not decodable WebP: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
		t.Errorf("thumb is %dx%d, want 256x256", b.Dx(), b.Dy())
	}
}

// The client turns this 404 into a placeholder tile. Answering anything else
// would make "not generated yet" look like a server fault.
func TestThumbIsNotFoundBeforeItIsGenerated(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.get(t, "/v1/assets/"+up.ID+"/thumb")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPreviewReturnsDecodableWebP(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.get(t, "/v1/assets/"+up.ID+"/preview")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/webp" {
		t.Errorf("Content-Type = %q, want image/webp", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if _, err := webp.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("preview is not decodable WebP: %v", err)
	}
}

// The preview is rendered per request, so the conditional check has to come
// before the conversion. Arrow-keying back through a viewer would otherwise
// cost an ImageMagick process per photo already in the browser's cache.
func TestPreviewAnswers304WithoutReconverting(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	first := h.get(t, "/v1/assets/"+up.ID+"/preview")
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("preview carried no ETag")
	}
	io.Copy(io.Discard, first.Body)

	second := h.getWith(t, "/v1/assets/"+up.ID+"/preview", map[string]string{"If-None-Match": etag})
	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.StatusCode)
	}
	body, _ := io.ReadAll(second.Body)
	if len(body) != 0 {
		t.Errorf("304 carried %d bytes of body", len(body))
	}
}

// Content-addressed bytes can never change under a client, so they are cached
// forever. Getting this wrong means re-converting every preview on every visit.
func TestDerivativesAreCachedImmutably(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	h.derive(t, up.ID)

	for _, path := range []string{"/thumb", "/preview", "/original"} {
		resp := h.get(t, "/v1/assets/"+up.ID+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d", path, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != immutable {
			t.Errorf("GET %s: Cache-Control = %q, want %q", path, cc, immutable)
		}
		if resp.Header.Get("ETag") == "" {
			t.Errorf("GET %s: no ETag", path)
		}
		io.Copy(io.Discard, resp.Body)
	}
}

func TestOriginalSupportsRangeRequests(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	up := decodeUpload(t, h.upload(t, content, nil))

	resp := h.getWith(t, "/v1/assets/"+up.ID+"/original", map[string]string{"Range": "bytes=0-99"})

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 100 {
		t.Errorf("got %d bytes, want 100", len(body))
	}
	if !bytes.Equal(body, content[:100]) {
		t.Error("ranged bytes do not match the original")
	}
}

func TestPreviewIsNotOfferedForVideo(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadNamedFixture(t, "clip.mov"), map[string]string{
		"X-Photo-Filename": "clip.mov",
	}))

	resp := h.get(t, "/v1/assets/"+up.ID+"/preview")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — videos are played, not previewed", resp.StatusCode)
	}
}

func TestPlaybackIsNotOfferedForPhotos(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.get(t, "/v1/assets/"+up.ID+"/playback")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// A video upload has to be classified as one on the way in, or the transcode is
// never queued and the viewer has nothing to play.
func TestVideoUploadIsClassifiedAsVideo(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadNamedFixture(t, "clip.mov"), map[string]string{
		"X-Photo-Filename": "clip.mov",
	}))

	var detail assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+up.ID), &detail)

	if detail.MediaKind != "video" {
		t.Errorf("kind = %q, want video", detail.MediaKind)
	}
}
