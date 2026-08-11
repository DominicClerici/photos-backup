package api

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/video"
)

const stillLocalID = "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"

// uploadLivePair uploads a Live Photo the way the phone does: the still, then
// the paired video declaring it.
func uploadLivePair(t *testing.T, h *harness) (stillID, videoID string) {
	t.Helper()

	still := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	paired := decodeUpload(t, h.upload(t, loadNamedFixture(t, "clip.mov"), map[string]string{
		"X-Photo-Filename":             "IMG_8071.MOV",
		"X-Photo-Local-Id":             stillLocalID + "#live",
		"X-Photo-Live-Parent-Local-Id": stillLocalID,
	}))
	return still.ID, paired.ID
}

// The whole point of the feature: one moment, one tile.
func TestLivePairShowsOneTimelineItem(t *testing.T) {
	h := newHarness(t)
	stillID, _ := uploadLivePair(t, h)

	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline"), &page)

	if len(page.Items) != 1 {
		t.Fatalf("got %d timeline items, want only the still", len(page.Items))
	}
	if page.Items[0].ID != stillID {
		t.Errorf("timeline showed %s, want the still %s", page.Items[0].ID, stillID)
	}
	if page.Items[0].LiveState != db.DerivedPending {
		t.Errorf("live = %q, want %q with the rendition queued", page.Items[0].LiveState, db.DerivedPending)
	}
}

// The pairing has to survive a database rebuilt from manifest.jsonl. Nothing in
// the bytes could recover it, so if the line does not carry it, a reindex puts
// every paired video back in the timeline.
func TestLivePairIsRecordedInTheManifest(t *testing.T) {
	h := newHarness(t)
	uploadLivePair(t, h)

	entries := h.manifestEntries(t)
	if len(entries) != 2 {
		t.Fatalf("got %d manifest lines, want 2", len(entries))
	}
	var declared int
	for _, e := range entries {
		if e.LiveParentLocalID == stillLocalID {
			declared++
		}
	}
	if declared != 1 {
		t.Errorf("%d manifest lines name the still, want exactly the paired video's", declared)
	}
}

func TestLiveThumbIsServedByTheStillsID(t *testing.T) {
	h := newHarness(t)
	stillID, videoID := uploadLivePair(t, h)
	writeLiveThumb(t, h, videoID)

	resp := h.get(t, "/v1/assets/"+stillID+"/live/thumb")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("the motion thumbnail came back empty")
	}
}

// A link or a second request may carry the video's own id. Answering it is free
// and refusing it would be a puzzle to debug.
func TestLiveThumbIsAlsoServedByTheVideosID(t *testing.T) {
	h := newHarness(t)
	_, videoID := uploadLivePair(t, h)
	writeLiveThumb(t, h, videoID)

	resp := h.get(t, "/v1/assets/"+videoID+"/live/thumb")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestLiveEndpointsRefuseAnAssetWithNoMotion(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	for _, path := range []string{"/live/thumb", "/live/preview"} {
		resp := h.get(t, "/v1/assets/"+up.ID+path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// Rendered per request, so this is the endpoint actually running ffmpeg.
func TestLivePreviewRendersOnDemand(t *testing.T) {
	h := newHarness(t)
	stillID, _ := uploadLivePair(t, h)

	resp := h.get(t, "/v1/assets/"+stillID+"/live/preview")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\n\nIs ffmpeg installed?", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Fatal("the live preview came back empty")
	}
	// Nothing is stored: the derivatives tree keeps the 256px rendition and
	// nothing larger.
	if h.srv.Derivatives.Exists(shaOf(t, h, stillID), derivstore.Playback) {
		t.Error("the live preview was written to disk; it is meant to be rendered per request")
	}
}

// A player asks for these more than once — Safari opens with a range probe
// before requesting the file properly. The second ask must not be a second
// ffmpeg.
func TestLivePreviewIsNotRenderedTwice(t *testing.T) {
	h := newHarness(t)
	stillID, videoID := uploadLivePair(t, h)

	first := h.get(t, "/v1/assets/"+stillID+"/live/preview")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\n\nIs ffmpeg installed?", first.StatusCode)
	}
	body, _ := io.ReadAll(first.Body)
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("the live preview carried no ETag, so a browser would re-fetch it every time")
	}

	// Conditional: answered before any work is considered.
	again := h.getWith(t, "/v1/assets/"+stillID+"/live/preview", map[string]string{"If-None-Match": etag})
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("conditional status = %d, want 304", again.StatusCode)
	}

	// Unconditional: answered from the cache, and byte-identical.
	third := h.get(t, "/v1/assets/"+videoID+"/live/preview")
	cached, _ := io.ReadAll(third.Body)
	if len(cached) != len(body) {
		t.Errorf("cached rendition is %d bytes, first was %d", len(cached), len(body))
	}
}

func TestLiveParentNamingItselfIsRejected(t *testing.T) {
	h := newHarness(t)

	resp := h.upload(t, loadNamedFixture(t, "clip.mov"), map[string]string{
		"X-Photo-Filename":             "IMG_8071.MOV",
		"X-Photo-Live-Parent-Local-Id": stillLocalID,
		"X-Photo-Local-Id":             stillLocalID,
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an asset cannot be its own Live Photo", resp.StatusCode)
	}
}

// writeLiveThumb runs the real encoder over the uploaded clip, standing in for
// the metadata worker the api package deliberately does not depend on.
func writeLiveThumb(t *testing.T, h *harness, videoID string) {
	t.Helper()
	ctx := context.Background()

	asset, err := h.store.Asset(ctx, videoID)
	if err != nil {
		t.Fatalf("load paired video: %v", err)
	}
	src := h.srv.Blobs.Path(asset.SHA256, asset.Ext)

	tool := video.New()
	info, err := tool.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probe: %v\n\nIs ffmpeg installed?", err)
	}
	staged, cleanup, err := h.srv.Derivatives.Stage("live-*" + derivstore.LiveThumb)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer cleanup()

	if err := tool.LiveThumb(ctx, src, staged, info); err != nil {
		t.Fatalf("LiveThumb: %v", err)
	}
	if err := h.srv.Derivatives.Commit(asset.SHA256, derivstore.LiveThumb, staged); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := h.store.SetLiveState(ctx, videoID, db.DerivedReady); err != nil {
		t.Fatalf("SetLiveState: %v", err)
	}
}

func shaOf(t *testing.T, h *harness, assetID string) string {
	t.Helper()
	asset, err := h.store.Asset(context.Background(), assetID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	return asset.SHA256
}
