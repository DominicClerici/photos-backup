package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

// seedThree uploads the three fixtures and returns their ids in timeline order.
func seedThree(t *testing.T, h *harness) []string {
	t.Helper()

	for i, name := range []string{"sample.heic", "photo.jpg", "bare.jpg"} {
		resp := h.upload(t, loadNamedFixture(t, name), map[string]string{
			"X-Photo-Filename":    name,
			"X-Photo-Local-Id":    fmt.Sprintf("LOCAL-%d/L0/001", i),
			"X-Photo-Captured-At": fmt.Sprintf("2026-08-0%dT12:00:00Z", 3-i),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s returned %d", name, resp.StatusCode)
		}
	}
	return listTimeline(t, h, "")
}

func listTimeline(t *testing.T, h *harness, query string) []string {
	t.Helper()
	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline"+query), &page)

	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.ID)
	}
	return out
}

func TestTrashEndpointMovesTheSelectionOutOfTheGallery(t *testing.T) {
	h := newHarness(t)
	ids := seedThree(t, h)

	var out trashResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash",
		fmt.Sprintf(`{"ids":[%q]}`, ids[1])), &out)

	if out.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", out.Deleted)
	}
	if out.Batch == "" {
		t.Error("no batch came back; the toast would have nothing to undo with")
	}

	if got := listTimeline(t, h, ""); len(got) != 2 || got[0] != ids[0] || got[1] != ids[2] {
		t.Errorf("timeline = %v, want %v", got, []string{ids[0], ids[2]})
	}
	if got := listTimeline(t, h, "?trash=1"); len(got) != 1 || got[0] != ids[1] {
		t.Errorf("trash = %v, want %v", got, []string{ids[1]})
	}
}

// The day table is what the grid lays itself out from, so it has to describe
// the trash the same way it describes the library — otherwise Recently Deleted
// could not reuse the gallery's grid at all.
func TestTrashScopeHasItsOwnDayTable(t *testing.T) {
	h := newHarness(t)
	ids := seedThree(t, h)

	h.postJSON(t, "/v1/trash", fmt.Sprintf(`{"ids":[%q]}`, ids[0]))

	var live, trash db.DayTable
	decodeJSON(t, h.get(t, "/v1/timeline/days?tz=UTC"), &live)
	decodeJSON(t, h.get(t, "/v1/timeline/days?tz=UTC&trash=1"), &trash)

	if live.Total != 2 {
		t.Errorf("library day table totals %d, want 2", live.Total)
	}
	if trash.Total != 1 {
		t.Errorf("trash day table totals %d, want 1", trash.Total)
	}
}

func TestUndoPutsTheWholeBatchBack(t *testing.T) {
	h := newHarness(t)
	ids := seedThree(t, h)

	var deleted trashResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash", `{"ranges":[{"start":0,"end":2}]}`), &deleted)
	if deleted.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted.Deleted)
	}

	var undone restoreResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash/restore",
		fmt.Sprintf(`{"batch":%q}`, deleted.Batch)), &undone)
	if undone.Restored != 2 {
		t.Errorf("restored = %d, want 2", undone.Restored)
	}

	if got := listTimeline(t, h, ""); len(got) != 3 {
		t.Errorf("timeline holds %d after the undo, want %v", len(got), ids)
	}
}

func TestRestoreByRangeReadsThePositionsInTheTrash(t *testing.T) {
	h := newHarness(t)
	seedThree(t, h)

	h.postJSON(t, "/v1/trash", `{"ranges":[{"start":0,"end":3}]}`)

	var undone restoreResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash/restore", `{"ranges":[{"start":0,"end":1}]}`), &undone)
	if undone.Restored != 1 {
		t.Errorf("restored = %d, want 1", undone.Restored)
	}
	if got := listTimeline(t, h, ""); len(got) != 1 {
		t.Errorf("timeline holds %d, want 1", len(got))
	}
}

// The purge is the only thing in this server that removes an original, so this
// is the test that says the bytes actually go.
func TestPurgeRemovesTheBlobAndTombstonesTheContent(t *testing.T) {
	h := newHarness(t)
	body := loadFixture(t)
	up := decodeUpload(t, h.upload(t, body, nil))

	if len(h.blobFiles(t)) != 1 {
		t.Fatalf("expected the upload to leave one blob, found %v", h.blobFiles(t))
	}

	h.postJSON(t, "/v1/trash", fmt.Sprintf(`{"ids":[%q]}`, up.ID))

	var out purgeResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash/purge", fmt.Sprintf(`{"ids":[%q]}`, up.ID)), &out)
	if out.Purged != 1 {
		t.Errorf("purged = %d, want 1", out.Purged)
	}
	if out.Bytes != int64(len(body)) {
		t.Errorf("freed %d bytes, want %d", out.Bytes, len(body))
	}

	if files := h.blobFiles(t); len(files) != 0 {
		t.Errorf("the original is still on disk: %v", files)
	}
	if resp := h.get(t, "/v1/assets/"+up.ID); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET the purged asset = %d, want 404", resp.StatusCode)
	}

	// The manifest is the copy that survives losing the database, so the purge
	// has to be in it or a rebuild would put the photograph back.
	var purges int
	for _, e := range h.manifestEntries(t) {
		if e.Type == manifest.KindPurge {
			purges++
		}
	}
	if purges != 1 {
		t.Errorf("manifest holds %d purge lines, want 1", purges)
	}
}

// A purge that could reach the library would make deleting a one-click loss.
func TestPurgeIgnoresAnythingNotInTheTrash(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	var out purgeResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash/purge", fmt.Sprintf(`{"ids":[%q]}`, up.ID)), &out)
	if out.Purged != 0 {
		t.Errorf("purged = %d from the live library, want 0", out.Purged)
	}
	if len(h.blobFiles(t)) != 1 {
		t.Error("the original was removed even though it was never deleted")
	}
}

// Purging is what makes a delete stick, and the only thing that can undo it is
// the phone quietly uploading the same bytes again on the next run.
func TestSyncCheckWillNotTakeBackPurgedContent(t *testing.T) {
	h := newHarness(t)
	body := loadFixture(t)
	up := decodeUpload(t, h.upload(t, body, nil))

	asset, err := h.store.Asset(t.Context(), up.ID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}

	h.postJSON(t, "/v1/trash", fmt.Sprintf(`{"ids":[%q]}`, up.ID))
	h.postJSON(t, "/v1/trash/purge", fmt.Sprintf(`{"ids":[%q]}`, up.ID))

	// A local id the server has never seen, carrying the content it just threw
	// away — which is exactly what a phone that still holds the photograph
	// sends on its next run.
	body_ := fmt.Sprintf(`{"deviceId":%q,"items":[{"localId":"NEW-ID/L0/001","md5":%q,"size":%d}]}`,
		h.deviceID, asset.MD5, asset.ByteSize)
	var out syncCheckResponse
	decodeJSON(t, h.postJSON(t, "/v1/sync/check", body_), &out)

	if len(out.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(out.Results))
	}
	if out.Results[0].Status != statusHave {
		t.Errorf("status = %q, want %q: the archive would take the photo back",
			out.Results[0].Status, statusHave)
	}
	if out.Results[0].AssetID != "" {
		t.Errorf("assetId = %q, want empty: there is nothing left to point at",
			out.Results[0].AssetID)
	}
}

func TestDeleteAlbumLeavesItsPhotosAlone(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	if resp := describeAsset(t, h, up.ID, takeoutSidecar); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import-metadata returned %d", resp.StatusCode)
	}

	var collections db.Collections
	decodeJSON(t, h.get(t, "/v1/collections"), &collections)
	if len(collections.Albums) != 1 {
		t.Fatalf("expected one album, got %v", collections.Albums)
	}
	album := collections.Albums[0].ID

	req, err := http.NewRequest(http.MethodDelete, h.server.URL+"/v1/collections/albums/"+album, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	h.authorize(req)

	var out trashResponse
	decodeJSON(t, h.do(t, req), &out)
	if out.Deleted != 0 {
		t.Errorf("deleted = %d photos, want 0", out.Deleted)
	}

	decodeJSON(t, h.get(t, "/v1/collections"), &collections)
	if len(collections.Albums) != 0 {
		t.Errorf("the album is still listed: %v", collections.Albums)
	}
	if got := listTimeline(t, h, ""); len(got) != 1 {
		t.Errorf("timeline holds %d, want 1: the photo should not have gone", len(got))
	}

	var undone restoreResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash/restore", fmt.Sprintf(`{"batch":%q}`, out.Batch)), &undone)
	if undone.Albums != 1 {
		t.Errorf("undo restored %d albums, want 1", undone.Albums)
	}
}

func TestDeleteAlbumWithItsPhotos(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	if resp := describeAsset(t, h, up.ID, takeoutSidecar); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("import-metadata returned %d", resp.StatusCode)
	}

	var collections db.Collections
	decodeJSON(t, h.get(t, "/v1/collections"), &collections)
	if len(collections.Albums) != 1 {
		t.Fatalf("expected one album, got %v", collections.Albums)
	}
	album := collections.Albums[0].ID

	req, err := http.NewRequest(http.MethodDelete,
		h.server.URL+"/v1/collections/albums/"+album+"?photos=true", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	h.authorize(req)

	var out trashResponse
	decodeJSON(t, h.do(t, req), &out)
	if out.Deleted != 1 {
		t.Errorf("deleted = %d photos, want 1", out.Deleted)
	}
	if got := listTimeline(t, h, ""); len(got) != 0 {
		t.Errorf("timeline holds %v, want nothing", got)
	}

	// One batch, so the album and its photographs come back together.
	var undone restoreResponse
	decodeJSON(t, h.postJSON(t, "/v1/trash/restore", fmt.Sprintf(`{"batch":%q}`, out.Batch)), &undone)
	if undone.Restored != 1 || undone.Albums != 1 {
		t.Errorf("undo = %d photos and %d albums, want 1 and 1", undone.Restored, undone.Albums)
	}
}

func TestCollectionsReportsTheTrashCount(t *testing.T) {
	h := newHarness(t)
	ids := seedThree(t, h)

	h.postJSON(t, "/v1/trash", fmt.Sprintf(`{"ids":[%q]}`, ids[0]))

	var collections db.Collections
	decodeJSON(t, h.get(t, "/v1/collections"), &collections)
	if collections.Trash != 1 {
		t.Errorf("trash = %d, want 1", collections.Trash)
	}
}

func TestSelectionMustNameSomething(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{`{}`, `{"ids":[],"ranges":[]}`} {
		resp := h.postJSON(t, "/v1/trash", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /v1/trash %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestSelectionRejectsTwoCollectionsAtOnce(t *testing.T) {
	h := newHarness(t)
	resp := h.postJSON(t, "/v1/trash",
		`{"ranges":[{"start":0,"end":1}],"album":"x","category":"videos"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The gallery reaches photod through the plaintext loopback listener, so its
// writes have to be served there or the browser cannot delete anything.
func TestTheGalleryCanDeleteWithASession(t *testing.T) {
	h := newHarness(t)
	ids := seedThree(t, h)
	gallery := h.gallery(t)

	resp, err := gallery.Client().Post(gallery.URL+"/v1/trash", "application/json",
		strings.NewReader(fmt.Sprintf(`{"ids":[%q]}`, ids[0])))
	if err != nil {
		t.Fatalf("POST /v1/trash: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out trashResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", out.Deleted)
	}
}

// The upload path names a device, and no amount of gallery writing may make it
// reachable by something that is not one. A session can trash a photograph and
// still cannot deliver one through the phone's endpoint.
func TestTheDeviceUploadPathRefusesASession(t *testing.T) {
	h := newHarness(t)
	gallery := h.gallery(t)

	resp, err := gallery.Client().Post(gallery.URL+"/v1/assets", "application/octet-stream",
		strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("POST /v1/assets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Errorf("a browser session was accepted on the device upload path: %d", resp.StatusCode)
	}
}
