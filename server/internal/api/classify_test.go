package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

// A Google Takeout export strips the extension off every Live Photo's paired
// video. Classified by name alone those become octet-stream images: the
// thumbnail fails, no playback rendition is ever queued, and the gallery shows
// an error tile. 216 of the 3,116 files in the test corpus arrive this way.
func TestUploadClassifiesExtensionlessVideoByContent(t *testing.T) {
	h := newHarness(t)

	resp := h.upload(t, loadNamedFixture(t, "clip.mov"), map[string]string{
		"X-Photo-Filename": "IMG_7266",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	got := decodeUpload(t, resp)

	asset, err := h.store.Asset(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if asset.ContentType != "video/quicktime" {
		t.Errorf("content type = %q, want video/quicktime", asset.ContentType)
	}
	if asset.MediaKind != db.MediaVideo {
		t.Errorf("media kind = %q, want video", asset.MediaKind)
	}
	if asset.Ext != ".mov" {
		t.Errorf("ext = %q, want .mov", asset.Ext)
	}

	// The sniffed extension has to reach the blob path too, or the worker hands
	// ffmpeg a file with no extension and the manifest cannot locate it later.
	blobs := h.blobFiles(t)
	if len(blobs) != 1 {
		t.Fatalf("blob tree holds %d files, want 1: %v", len(blobs), blobs)
	}
	if want := filepath.Join(got.SHA256[0:2], got.SHA256[2:4], got.SHA256+".mov"); blobs[0] != want {
		t.Errorf("blob at %q, want %q", blobs[0], want)
	}

	entries := h.manifestEntries(t)
	if len(entries) != 1 {
		t.Fatalf("manifest holds %d lines, want 1", len(entries))
	}
	if entries[0].ContentType != "video/quicktime" || entries[0].Ext != ".mov" {
		t.Errorf("manifest recorded %q/%q, want video/quicktime/.mov",
			entries[0].ContentType, entries[0].Ext)
	}
}

// A recognised extension is authoritative, so classification costs nothing for
// the overwhelming majority of uploads.
func TestUploadTrustsKnownExtension(t *testing.T) {
	h := newHarness(t)

	got := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	asset, err := h.store.Asset(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if asset.ContentType != "image/heic" {
		t.Errorf("content type = %q, want image/heic", asset.ContentType)
	}
	if asset.MediaKind != db.MediaImage {
		t.Errorf("media kind = %q, want image", asset.MediaKind)
	}
}

// A file neither the name nor the bytes identify is still archived. Refusing it
// would mean the backup silently skips something the phone holds.
func TestUploadStoresUnclassifiableBytes(t *testing.T) {
	h := newHarness(t)

	resp := h.upload(t, []byte("not a photo, not a video, still someone's file"), map[string]string{
		"X-Photo-Filename": "mystery.bin",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	got := decodeUpload(t, resp)

	asset, err := h.store.Asset(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if asset.ContentType != "application/octet-stream" {
		t.Errorf("content type = %q, want application/octet-stream", asset.ContentType)
	}
	if asset.MediaKind != db.MediaImage {
		t.Errorf("media kind = %q, want image", asset.MediaKind)
	}
}
