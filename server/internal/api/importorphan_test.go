package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

// The body of a sidecar Google wrote for a file that is in one of the other
// five zips. It is the whole reason this endpoint exists: everything here —
// the caption, the person, the coordinates, the capture time — exists nowhere
// but in this JSON, and the export is deleted the week after the import.
const orphanedSidecar = `{
  "title": "IMG_9001.HEIC",
  "description": "the last morning",
  "photoTakenTime": { "timestamp": "1736125085" },
  "geoData": { "latitude": 41.7844, "longitude": -122.5848 },
  "people": [{ "name": "Brody" }]
}`

func recordOrphan(t *testing.T, h *harness, payload map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return h.postJSON(t, "/v1/import/orphans", string(body))
}

func sidecarOrphan() map[string]any {
	return map[string]any{
		"source":  "google-takeout",
		"kind":    "sidecar",
		"locator": "Photos from 2025/IMG_9001.HEIC.supplemental-metadata.json",
		"sidecar": json.RawMessage(orphanedSidecar),
		"reason":  "no media file in this export matched the sidecar",
	}
}

func TestImportOrphanKeepsAnUnmatchedSidecarWhole(t *testing.T) {
	h := newHarness(t)

	if resp := recordOrphan(t, h, sidecarOrphan()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import/orphans returned %d, want 204", resp.StatusCode)
	}

	counts, err := h.store.ImportOrphanCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if counts["sidecar"] != 1 {
		t.Errorf("sidecar orphans = %d, want 1", counts["sidecar"])
	}
}

// Re-running an import is how anyone recovers a half-finished one, and it
// re-reads every sidecar it could not place last time.
func TestImportOrphanIsIdempotent(t *testing.T) {
	h := newHarness(t)

	for range 3 {
		if resp := recordOrphan(t, h, sidecarOrphan()); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("import/orphans returned %d, want 204", resp.StatusCode)
		}
	}

	counts, err := h.store.ImportOrphanCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if counts["sidecar"] != 1 {
		t.Errorf("sidecar orphans = %d after three identical runs, want 1", counts["sidecar"])
	}
}

// An item in an album folder whose sidecar did not match. The asset is in the
// archive; what could not be recorded is that it belonged to an album, and
// album membership exists nowhere in an export but the directory layout.
func TestImportOrphanRecordsAlbumsThatCouldNotBeApplied(t *testing.T) {
	h := newHarness(t)
	uploaded := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := recordOrphan(t, h, map[string]any{
		"source":  "google-takeout",
		"kind":    "album",
		"locator": "Archive/IMG_9002.HEIC",
		"assetId": uploaded.ID,
		"albums":  []map[string]string{{"title": "Archive"}},
		"reason":  "no sidecar matched this item",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import/orphans returned %d, want 204", resp.StatusCode)
	}

	counts, err := h.store.ImportOrphanCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if counts["album"] != 1 {
		t.Errorf("album orphans = %d, want 1", counts["album"])
	}

	// Recorded, deliberately not applied: an orphan is evidence, and putting
	// the album on the asset here would be the archive guessing.
	var detail assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+uploaded.ID), &detail)
	if len(detail.Albums) != 0 {
		t.Errorf("albums = %v, want none — an orphan is not a decision", detail.Albums)
	}
}

// The manifest is the point. A sidecar orphan names no blob, so nothing else in
// the archive refers to it, and a lost database would take the only surviving
// copy of what Google knew about that photograph with it.
func TestImportOrphanIsRecordedInTheManifest(t *testing.T) {
	h := newHarness(t)
	recordOrphan(t, h, sidecarOrphan())

	entries := h.manifestEntries(t)
	if len(entries) != 1 {
		t.Fatalf("got %d manifest lines, want one orphan line", len(entries))
	}

	line := entries[0]
	if line.Type != manifest.KindImportOrphan {
		t.Fatalf("line is %q, want %q", line.Type, manifest.KindImportOrphan)
	}
	// It must not read as an asset line, or a rebuild invents an archived file
	// with no size, no type and no bytes on disk.
	if line.IsAsset() {
		t.Error("an orphan line reads as an asset line")
	}
	if line.OrphanKind != "sidecar" {
		t.Errorf("orphan kind = %q", line.OrphanKind)
	}
	if line.Locator == "" {
		t.Error("the line has no locator, so a re-import cannot match it to this row")
	}
	if len(line.ImportSidecar) == 0 {
		t.Fatal("the sidecar body was not written to the manifest")
	}
	var sidecar map[string]any
	if err := json.Unmarshal(line.ImportSidecar, &sidecar); err != nil {
		t.Fatalf("the stored sidecar is not JSON: %v", err)
	}
	if sidecar["description"] != "the last morning" {
		t.Errorf("the sidecar reached the manifest reduced, not verbatim: %v", sidecar)
	}
}

func TestImportOrphanRefusesWhatItCannotRead(t *testing.T) {
	h := newHarness(t)

	for name, payload := range map[string]map[string]any{
		"unknown source": {"source": "icloud", "kind": "sidecar", "locator": "a.json",
			"sidecar": json.RawMessage(`{}`)},
		"unknown kind": {"source": "google-takeout", "kind": "vibes", "locator": "a.json",
			"sidecar": json.RawMessage(`{}`)},
		"no locator": {"source": "google-takeout", "kind": "sidecar", "locator": "",
			"sidecar": json.RawMessage(`{}`)},
		"a sidecar orphan with no sidecar": {"source": "google-takeout", "kind": "sidecar",
			"locator": "a.json"},
		"an album orphan with no albums": {"source": "google-takeout", "kind": "album",
			"locator": "a.HEIC"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := recordOrphan(t, h, payload)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("returned %d, want 400", resp.StatusCode)
			}
		})
	}

	// Nothing refused reached the log, which is what keeps the manifest a
	// record of the archive rather than of the requests made to it.
	if entries := h.manifestEntries(t); len(entries) != 0 {
		t.Errorf("%d manifest lines written for refused requests", len(entries))
	}
}
