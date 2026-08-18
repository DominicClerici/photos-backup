package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

const takeoutSidecar = `{
  "title": "IMG_5874.HEIC",
  "description": "at the border",
  "photoTakenTime": { "timestamp": "1736125085" },
  "geoData": { "latitude": 41.7844, "longitude": -122.5848 },
  "favorited": true,
  "people": [{ "name": "Brody" }]
}`

func describeAsset(t *testing.T, h *harness, assetID, sidecar string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"source":  "google-takeout",
		"sidecar": json.RawMessage(sidecar),
		"albums":  []map[string]string{{"title": "Iceland 2025", "description": "the ring road"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h.postJSON(t, "/v1/assets/"+assetID+"/import-metadata", string(body))
}

func TestImportMetadataIsAppliedAndReadableOnTheAsset(t *testing.T) {
	h := newHarness(t)
	uploaded := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	if resp := describeAsset(t, h, uploaded.ID, takeoutSidecar); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import-metadata returned %d, want 204", resp.StatusCode)
	}

	var detail assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+uploaded.ID), &detail)

	if detail.Description != "at the border" {
		t.Errorf("description = %q", detail.Description)
	}
	if !detail.Favorite {
		t.Error("favorite = false")
	}
	if len(detail.Albums) != 1 || detail.Albums[0] != "Iceland 2025" {
		t.Errorf("albums = %v", detail.Albums)
	}
	if len(detail.People) != 1 || detail.People[0] != "Brody" {
		t.Errorf("people = %v", detail.People)
	}
}

// The sidecar has to reach the manifest, or a rebuilt database loses everything
// the export knew — and the export is usually deleted by then.
func TestImportMetadataIsRecordedInTheManifest(t *testing.T) {
	h := newHarness(t)
	uploaded := decodeUpload(t, h.upload(t, loadFixture(t), nil))
	describeAsset(t, h, uploaded.ID, takeoutSidecar)

	entries := h.manifestEntries(t)
	if len(entries) != 2 {
		t.Fatalf("got %d manifest lines, want the asset line and a metadata line", len(entries))
	}

	line := entries[1]
	if line.Type != manifest.KindMetadata || line.IsAsset() {
		t.Fatalf("second line is %q, want %q", line.Type, manifest.KindMetadata)
	}
	if line.SHA256 != uploaded.SHA256 {
		t.Errorf("metadata line names %s, want %s", line.SHA256, uploaded.SHA256)
	}
	if len(line.ImportSidecar) == 0 {
		t.Error("the sidecar was not written to the manifest")
	}
	if len(line.ImportAlbums) != 1 || line.ImportAlbums[0].Title != "Iceland 2025" {
		t.Errorf("albums = %v, want them on the line too: the directory layout they came from is gone", line.ImportAlbums)
	}
	// The asset line stays an asset line, so a rebuild still finds the blob.
	if !entries[0].IsAsset() {
		t.Error("the upload's own line stopped being an asset line")
	}
}

func TestImportMetadataRefusesAFormatItCannotRead(t *testing.T) {
	h := newHarness(t)
	uploaded := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.postJSON(t, "/v1/assets/"+uploaded.ID+"/import-metadata",
		`{"source":"apple-photos","sidecar":{"title":"x"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown source returned %d, want 400", resp.StatusCode)
	}

	if entries := h.manifestEntries(t); len(entries) != 1 {
		t.Errorf("got %d manifest lines, want nothing written for a rejected request", len(entries))
	}
}

func TestImportMetadataOnAnUnknownAssetIs404(t *testing.T) {
	h := newHarness(t)
	resp := describeAsset(t, h, "6f2b8c1e-0000-4000-8000-000000000000", takeoutSidecar)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("returned %d, want 404", resp.StatusCode)
	}
}

// It is a write, and writes are not reachable on the listener that carries no
// credentials.
func TestImportMetadataIsRefusedOnThePlaintextListener(t *testing.T) {
	h := newHarness(t)
	uploaded := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	plain := h.plaintext(t)
	resp, err := http.Post(plain.URL+"/v1/assets/"+uploaded.ID+"/import-metadata",
		"application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Error("the plaintext listener accepted a write")
	}
}
