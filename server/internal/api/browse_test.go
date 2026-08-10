package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

func TestTimelineListsUploadedAssets(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline"), &page)

	if len(page.Items) != 1 {
		t.Fatalf("timeline holds %d items, want 1", len(page.Items))
	}
	item := page.Items[0]
	if item.ID != up.ID {
		t.Errorf("item id = %q, want %q", item.ID, up.ID)
	}
	if item.MediaKind != db.MediaImage {
		t.Errorf("kind = %q, want image", item.MediaKind)
	}
	// The tile has to appear before its thumbnail exists, or most of the
	// library is invisible during a backfill.
	if item.State != db.DerivedPending {
		t.Errorf("state = %q, want pending", item.State)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q on a single-page result", page.NextCursor)
	}
}

func TestTimelineIsEmptyArrayNotNull(t *testing.T) {
	h := newHarness(t)

	// A null items field would make the client special-case the empty archive.
	var raw struct {
		Items []db.TimelineItem `json:"items"`
	}
	resp := h.get(t, "/v1/timeline")
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw.Items == nil {
		t.Error("items is null on an empty archive, want []")
	}
}

func TestTimelinePagesWithACursor(t *testing.T) {
	h := newHarness(t)

	ids := make(map[string]bool)
	for _, name := range []string{"sample.heic", "photo.jpg", "bare.jpg"} {
		up := decodeUpload(t, h.upload(t, loadNamedFixture(t, name), map[string]string{
			"X-Photo-Filename": name,
			"X-Photo-Local-Id": name,
		}))
		ids[up.ID] = true
	}

	var first db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?limit=2"), &first)
	if len(first.Items) != 2 {
		t.Fatalf("first page holds %d items, want 2", len(first.Items))
	}
	if first.NextCursor == "" {
		t.Fatal("first page carried no cursor with more items outstanding")
	}

	var second db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?cursor="+first.NextCursor), &second)
	if len(second.Items) != 1 {
		t.Fatalf("second page holds %d items, want 1", len(second.Items))
	}

	seen := make(map[string]bool)
	for _, it := range append(first.Items, second.Items...) {
		if seen[it.ID] {
			t.Errorf("asset %s appeared on both pages", it.ID)
		}
		seen[it.ID] = true
	}
	if len(seen) != len(ids) {
		t.Errorf("paged through %d assets, want %d", len(seen), len(ids))
	}
}

func TestTimelineRejectsABadCursorAndLimit(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{
		"/v1/timeline?cursor=nonsense!!",
		"/v1/timeline?limit=0",
		"/v1/timeline?limit=9999",
		"/v1/timeline?limit=abc",
	} {
		if resp := h.get(t, path); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestTimelineStatesReportsProgress(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	var before struct {
		Items []db.TimelineItem `json:"items"`
	}
	decodeJSON(t, h.postJSON(t, "/v1/timeline/states", `{"ids":["`+up.ID+`"]}`), &before)
	if len(before.Items) != 1 || before.Items[0].State != db.DerivedPending {
		t.Fatalf("states before deriving = %+v, want one pending", before.Items)
	}

	h.derive(t, up.ID)

	var after struct {
		Items []db.TimelineItem `json:"items"`
	}
	decodeJSON(t, h.postJSON(t, "/v1/timeline/states", `{"ids":["`+up.ID+`"]}`), &after)
	if len(after.Items) != 1 || after.Items[0].State != db.DerivedReady {
		t.Fatalf("states after deriving = %+v, want one ready", after.Items)
	}
}

func TestTimelineStatesRejectsBadInput(t *testing.T) {
	h := newHarness(t)

	if resp := h.postJSON(t, "/v1/timeline/states", `not json`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d for a malformed body, want 400", resp.StatusCode)
	}
	if resp := h.postJSON(t, "/v1/timeline/states", `{"ids":["nope"]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d for a malformed id, want 400", resp.StatusCode)
	}
}

func TestAssetDetailCarriesTheMetadataPanelFields(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadNamedFixture(t, "iphone-portrait.heic"), map[string]string{
		"X-Photo-Filename": "iphone-portrait.heic",
	}))
	h.derive(t, up.ID)

	var got assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+up.ID), &got)

	if got.Filename != "iphone-portrait.heic" {
		t.Errorf("filename = %q", got.Filename)
	}
	if got.CameraModel != "iPhone 14 Pro" {
		t.Errorf("camera_model = %q", got.CameraModel)
	}
	if got.Width == nil || *got.Width != 3024 {
		t.Errorf("width = %v, want the display width 3024", got.Width)
	}
	if got.GPSLat == nil || got.GPSLon == nil {
		t.Error("detail carried no GPS")
	}
	if got.OffsetMinutes == nil || *got.OffsetMinutes != -240 {
		t.Errorf("offset_minutes = %v, want -240", got.OffsetMinutes)
	}
	// The phone's value is kept beside the file's rather than overwritten.
	if got.ReportedAt == nil {
		t.Error("detail dropped the capture time the phone reported")
	}
	if got.State != db.DerivedReady {
		t.Errorf("state = %q, want ready", got.State)
	}
}

func TestJobsSummaryReportsQueueDepth(t *testing.T) {
	h := newHarness(t)
	h.upload(t, loadFixture(t), nil)

	var got jobsSummary
	decodeJSON(t, h.get(t, "/v1/jobs"), &got)

	var pending int64
	for _, c := range got.Counts {
		pending += c.Count
	}
	if pending != 1 {
		t.Errorf("queue holds %d jobs, want 1 for one upload", pending)
	}
	if got.Failed == nil {
		t.Error("failed is null, want []")
	}
}

func TestHealthCountsPendingAndFailedWork(t *testing.T) {
	h := newHarness(t)
	h.upload(t, loadFixture(t), nil)

	var got health
	decodeJSON(t, h.get(t, "/health"), &got)

	if !got.OK {
		t.Error("ok = false")
	}
	if got.PendingJobs != 1 {
		t.Errorf("pending_jobs = %d, want 1", got.PendingJobs)
	}
	if got.FailedJobs != 0 {
		t.Errorf("failed_jobs = %d, want 0", got.FailedJobs)
	}
}
