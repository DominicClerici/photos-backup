package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"testing"

	"golang.org/x/image/webp"
)

// layerBytes is a caption layer: opaque red over its left half, transparent
// over its right, at a size that matches no fixture — which is the position
// every Snapchat memory is in.
func layerBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width / 2 {
			img.Set(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("encode layer: %v", err)
	}
	return out.Bytes()
}

// memory uploads a photograph and a layer and links them, which is the state
// `photobackup import-snapchat` leaves behind.
func (h *harness) memory(t *testing.T) (photoID string) {
	t.Helper()

	photo := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	layer := decodeUpload(t, h.upload(t, layerBytes(t, 101, 253),
		map[string]string{"X-Photo-Filename": "2017-09-02_abc-overlay.png"}))

	if err := h.store.LinkOverlay(context.Background(), photo.ID, layer.SHA256); err != nil {
		t.Fatalf("LinkOverlay: %v", err)
	}
	h.derive(t, photo.ID)
	return photo.ID
}

// redAt reads one pixel out of a rendition the server returned.
func redAt(t *testing.T, resp *http.Response, x, y int) uint8 {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	img, err := webp.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not decodable WebP: %v", err)
	}
	r, _, _, _ := img.At(x, y).RGBA()
	return uint8(r >> 8)
}

// The default preview is the composite. Everything that just asks for a
// preview — the phone app, a saved link, the gallery — gets the picture that
// was actually sent rather than the photograph Snapchat shipped underneath it.
func TestPreviewComposesTheOverlayByDefault(t *testing.T) {
	h := newHarness(t)
	id := h.memory(t)

	resp := h.get(t, "/v1/assets/"+id+"/preview")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if red := redAt(t, resp, 40, 150); red < 180 {
		t.Errorf("preview reads red=%d where the layer is; it was rendered from the photograph alone", red)
	}
}

// And the plain route is the photograph underneath, which is what the viewer's
// press-and-hold and its toggle reach for.
func TestPreviewPlainLeavesTheOverlayOut(t *testing.T) {
	h := newHarness(t)
	id := h.memory(t)

	resp := h.get(t, "/v1/assets/"+id+"/preview/plain")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if red := redAt(t, resp, 40, 150); red > 180 {
		t.Errorf("plain preview reads red=%d; the layer was drawn into it anyway", red)
	}
}

// The two are different pictures, so they must not share an ETag. They are both
// served immutable: a browser handed one under the other's tag would keep it
// for a year.
func TestPreviewVariantsDoNotShareAnETag(t *testing.T) {
	h := newHarness(t)
	id := h.memory(t)

	composite := h.get(t, "/v1/assets/"+id+"/preview").Header.Get("ETag")
	plain := h.get(t, "/v1/assets/"+id+"/preview/plain").Header.Get("ETag")
	if composite == "" || plain == "" {
		t.Fatalf("a preview was served without an ETag (composite %q, plain %q)", composite, plain)
	}
	if composite == plain {
		t.Errorf("both previews are tagged %q, so one will be served as the other", composite)
	}

	// And the conditional check still short-circuits, which is what keeps
	// arrow-keying back through a viewer from costing an ImageMagick each time.
	again := h.getWith(t, "/v1/assets/"+id+"/preview", map[string]string{"If-None-Match": composite})
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304 for a client that already has these bytes", again.StatusCode)
	}
}

// A photograph nobody drew on answers the plain route with itself. The URL
// means "the photograph", and that is a true answer for one with no layer.
func TestPreviewPlainAnswersForAnAssetWithNoOverlay(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	h.derive(t, up.ID)

	resp := h.get(t, "/v1/assets/"+up.ID+"/preview/plain")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
}

// The plain playback rendition is a file that only exists for videos carrying a
// layer. Answering with the burned one would pin a composite in a browser's
// immutable cache under the URL that promises not to be one.
func TestPlaybackPlainRefusesAVideoWithNoOverlay(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadNamedFixture(t, "clip.mov"),
		map[string]string{"X-Photo-Filename": "clip.mov"}))

	resp := h.get(t, "/v1/assets/"+up.ID+"/playback/plain")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a video with nothing to leave out", resp.StatusCode)
	}
}

// The viewer offers the toggle on the strength of this one field, so it has to
// be there for a memory and absent for everything else.
func TestAssetDetailReportsAnOverlay(t *testing.T) {
	h := newHarness(t)
	id := h.memory(t)

	if !hasOverlay(t, h, id) {
		t.Error("has_overlay is false for a memory that carries one")
	}

	plain := decodeUpload(t, h.upload(t, loadNamedFixture(t, "photo.jpg"),
		map[string]string{"X-Photo-Filename": "photo.jpg"}))
	if hasOverlay(t, h, plain.ID) {
		t.Error("has_overlay is true for a photograph nobody drew on")
	}
}

// hasOverlay reads the field a viewer decides on. The zero value stands in for
// an absent one, which is what the gallery's own JSON parsing does with an
// omitempty field.
func hasOverlay(t *testing.T, h *harness, id string) bool {
	t.Helper()
	var detail struct {
		HasOverlay bool `json:"has_overlay"`
	}
	if err := json.NewDecoder(h.get(t, "/v1/assets/"+id).Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return detail.HasOverlay
}
