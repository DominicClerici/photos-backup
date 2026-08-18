package worker

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"golang.org/x/image/webp"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// layer archives a caption layer and links it to a photograph, which is exactly
// what `photobackup import-snapchat` does: two ordinary assets and one row
// pointing at the other.
//
// It is opaque red over its left half and transparent over its right, at a size
// and shape that match nothing — Snapchat's layer is the phone's screen and the
// media is what filled it, and across this archive's 439 memories the two never
// once agree on dimensions.
func (h *harness) layer(t *testing.T, photo db.Asset, width, height int) db.Asset {
	t.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width / 2 {
			img.Set(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var body bytes.Buffer
	if err := png.Encode(&body, img); err != nil {
		t.Fatalf("encode layer: %v", err)
	}

	over := h.ingestBytes(t, "2017-09-02_abc-overlay.png", db.MediaImage, body.Bytes())
	if err := h.store.LinkOverlay(context.Background(), photo.ID, over.SHA256); err != nil {
		t.Fatalf("LinkOverlay: %v", err)
	}
	return over
}

// redAt reads one pixel out of a stored thumbnail. Whether the layer was drawn
// is not something a file size or an exit code can answer.
func redAt(t *testing.T, path string, x, y int) uint8 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendition: %v", err)
	}
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("rendition is not decodable WebP: %v", err)
	}
	r, _, _, _ := img.At(x, y).RGBA()
	return uint8(r >> 8)
}

// The thumbnail the grid draws is of the composite, not of the photograph. This
// is the whole feature: the picture that was sent exists in neither archived
// file, and the pipeline is the only place it is ever assembled.
func TestMetadataJobComposesTheLayerIntoEveryThumbnail(t *testing.T) {
	h := newHarness(t)
	photo := h.ingest(t, "photo.jpg", db.MediaImage)
	h.layer(t, photo, 101, 253)

	// Reloaded, because linking happened after the row was read and the job
	// reads the row itself.
	h.claimAndRun(t, jobs.KindMetadata)

	got := h.reload(t, photo.ID)
	if got.DerivedState != db.DerivedReady {
		t.Fatalf("DerivedState = %q, want ready", got.DerivedState)
	}
	if got.OverlayAssetID == nil {
		t.Fatal("the photograph lost its link to the layer")
	}

	for _, size := range derivstore.ThumbSizes {
		path := h.Derivatives.Path(photo.SHA256, derivstore.ThumbSuffix(size))
		if stat, err := os.Stat(path); err != nil || stat.Size() == 0 {
			t.Fatalf("%dpx thumbnail missing or empty: %v", size, err)
		}
		// The layer covers the left half of the frame, and a square crop of a
		// landscape photograph keeps its middle — so a tenth of the way in is
		// inside the red either way.
		if red := redAt(t, path, size/10, size/2); red < 180 {
			t.Errorf("%dpx thumbnail reads red=%d where the layer is; it was built from the photograph alone",
				size, red)
		}
		if red := redAt(t, path, size-size/10, size/2); red > 180 {
			t.Errorf("%dpx thumbnail is red where the layer is transparent: red=%d", size, red)
		}
	}
}

// The layer itself is an ordinary asset with its own thumbnails. It is kept out
// of the timeline by is_overlay rather than by having nothing to draw, so a
// pipeline that skipped it would be hiding a bug rather than a picture.
func TestMetadataJobStillRendersTheLayerItself(t *testing.T) {
	h := newHarness(t)
	photo := h.ingest(t, "photo.jpg", db.MediaImage)
	over := h.layer(t, photo, 101, 253)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMetadata)

	if !h.reload(t, over.ID).IsOverlay {
		t.Error("the layer is not marked as one, so the timeline will draw it")
	}
	path := h.Derivatives.Path(over.SHA256, derivstore.Thumb)
	if stat, err := os.Stat(path); err != nil || stat.Size() == 0 {
		t.Errorf("the layer got no thumbnail of its own: %v", err)
	}
}

// A video tile is a poster frame, and it has to carry the caption for the same
// reason the still does — otherwise the grid shows one picture and the viewer
// shows another.
func TestMetadataJobComposesTheLayerIntoAVideoPoster(t *testing.T) {
	h := newHarness(t)
	clip := h.ingest(t, "clip.mov", db.MediaVideo)
	h.layer(t, clip, 337, 601)

	h.claimAndRun(t, jobs.KindMetadata)

	path := h.Derivatives.Path(clip.SHA256, derivstore.Thumb)
	if stat, err := os.Stat(path); err != nil || stat.Size() == 0 {
		t.Fatalf("poster thumbnail missing or empty: %v", err)
	}
	if red := redAt(t, path, 25, 128); red < 180 {
		t.Errorf("the poster reads red=%d where the layer is; it was built from the frame alone", red)
	}
}

// Two renditions for a video that carries a layer: the composite everything
// gets by default, and the photograph underneath for the viewer's toggle.
// Nothing on the client can lay a PNG over a playing video, which is why this
// one costs a second encode where a still costs nothing.
func TestPlaybackJobWritesBothTheBurnedAndThePlainRendition(t *testing.T) {
	h := newHarness(t)
	clip := h.ingest(t, "clip.mov", db.MediaVideo)
	h.layer(t, clip, 337, 601)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindPlayback)

	if state := h.reload(t, clip.ID).PlaybackState; state != db.DerivedReady {
		t.Fatalf("PlaybackState = %q, want ready", state)
	}

	for _, suffix := range []string{derivstore.Playback, derivstore.PlaybackPlain} {
		path := h.Derivatives.Path(clip.SHA256, suffix)
		if stat, err := os.Stat(path); err != nil || stat.Size() == 0 {
			t.Fatalf("%s rendition missing or empty: %v", suffix, err)
		}
		info, err := h.Video.Probe(context.Background(), path)
		if err != nil {
			t.Fatalf("probe %s: %v", suffix, err)
		}
		if info.Width != 640 || info.Height != 480 {
			t.Errorf("%s is %dx%d, want the source's 640x480", suffix, info.Width, info.Height)
		}
	}
}

// And a video with no layer keeps exactly one rendition. The second file is for
// the few hundred memories that need it, not for the library.
func TestPlaybackJobWritesOnlyOneRenditionWithoutALayer(t *testing.T) {
	h := newHarness(t)
	clip := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindPlayback)

	if h.Derivatives.Exists(clip.SHA256, derivstore.PlaybackPlain) {
		t.Error("a video with no overlay got a second rendition of itself")
	}
}
