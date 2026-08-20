package api

import (
	"context"
	"encoding/json"
	"fmt"
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

// The day table is what the gallery lays its whole grid out from, so the
// numbers in it have to describe the pages the same endpoint will serve.
func TestTimelineDaysDescribesThePagesItWillServe(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"sample.heic", "photo.jpg", "bare.jpg"} {
		h.upload(t, loadNamedFixture(t, name), map[string]string{
			"X-Photo-Filename": name,
			"X-Photo-Local-Id": name,
		})
	}

	var table db.DayTable
	decodeJSON(t, h.get(t, "/v1/timeline/days?tz=UTC"), &table)

	if table.Zone != "UTC" {
		t.Errorf("tz = %q, want UTC", table.Zone)
	}
	var counted int
	for _, day := range table.Days {
		counted += day.Count
	}
	if counted != table.Total {
		t.Errorf("run lengths sum to %d, total says %d", counted, table.Total)
	}

	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?limit=500"), &page)
	if table.Total != len(page.Items) {
		t.Errorf("total = %d, timeline served %d items", table.Total, len(page.Items))
	}
}

// A timezone the server cannot resolve must not take the page down with it.
// The days it produces are then wrong by a few hours for files that recorded no
// zone of their own, which is a far smaller failure than a gallery that will
// not open.
func TestTimelineDaysFallsBackOnAnUnknownTimezone(t *testing.T) {
	h := newHarness(t)
	h.upload(t, loadFixture(t), nil)

	var table db.DayTable
	decodeJSON(t, h.get(t, "/v1/timeline/days?tz=Nowhere%2FAtlantis"), &table)

	if table.Zone != "UTC" {
		t.Errorf("tz = %q, want the UTC fallback", table.Zone)
	}
	if table.Total != 1 {
		t.Errorf("total = %d, want 1", table.Total)
	}
}

func TestTimelineDaysRejectsTwoCollections(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/v1/timeline/days?album=x&category=videos")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A fling into the middle of the library asks for a row offset rather than a
// cursor, and has to land on the row the day table counted.
func TestTimelineSkipsToARowOffset(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"sample.heic", "photo.jpg", "bare.jpg"} {
		h.upload(t, loadNamedFixture(t, name), map[string]string{
			"X-Photo-Filename": name,
			"X-Photo-Local-Id": name,
		})
	}

	var whole db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?limit=500"), &whole)

	var jumped db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?skip=2&limit=500"), &jumped)

	if len(jumped.Items) != len(whole.Items)-2 {
		t.Fatalf("skipped page holds %d items, want %d", len(jumped.Items), len(whole.Items)-2)
	}
	if jumped.Items[0].ID != whole.Items[2].ID {
		t.Errorf("skip=2 landed on %q, want %q", jumped.Items[0].ID, whole.Items[2].ID)
	}
}

// The two ways of saying where a page starts mean different things and cannot
// both be honoured, so naming both is a mistake worth reporting rather than one
// to resolve by precedence.
func TestTimelineRefusesACursorAndASkipTogether(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"sample.heic", "photo.jpg"} {
		h.upload(t, loadNamedFixture(t, name), map[string]string{
			"X-Photo-Filename": name,
			"X-Photo-Local-Id": name,
		})
	}

	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?limit=1"), &page)
	if page.NextCursor == "" {
		t.Fatal("no cursor to pair the skip with")
	}

	resp := h.get(t, "/v1/timeline?skip=1&cursor="+page.NextCursor)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTimelineRejectsANegativeSkip(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/v1/timeline?skip=-1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A shared link carries an id and the grid is addressed by position, so this is
// the translation between them — and the number it gives back has to be a
// position in the very pages the gallery fetches.
func TestTimelineLocateGivesAPositionInTheTimeline(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"sample.heic", "photo.jpg", "bare.jpg"} {
		h.upload(t, loadNamedFixture(t, name), map[string]string{
			"X-Photo-Filename": name,
			"X-Photo-Local-Id": name,
		})
	}

	var whole db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline?limit=500"), &whole)

	for want := range whole.Items {
		var found struct {
			Index int `json:"index"`
		}
		decodeJSON(t, h.get(t, "/v1/timeline/locate?id="+whole.Items[want].ID), &found)
		if found.Index != want {
			t.Errorf("item %d located at %d", want, found.Index)
		}

		// And the position resolves back to the same asset through the pages.
		var page db.TimelinePage
		decodeJSON(t, h.get(t, fmt.Sprintf("/v1/timeline?skip=%d&limit=1", found.Index)), &page)
		if len(page.Items) != 1 || page.Items[0].ID != whole.Items[want].ID {
			t.Errorf("skip=%d did not land back on item %d", found.Index, want)
		}
	}
}

// A link to a photo that is not in the collection being browsed is the ordinary
// case — an album page handed a library link — and it is a 404 rather than a
// position in some other timeline.
func TestTimelineLocateRefusesWhatIsNotInTheCollection(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.get(t, "/v1/timeline/locate?id="+up.ID+"&category=videos")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTimelineLocateRejectsAMissingID(t *testing.T) {
	h := newHarness(t)
	if resp := h.get(t, "/v1/timeline/locate"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Both an unknown asset and an unparseable one mean the same thing to the
// caller: there is no position here to go to.
func TestTimelineLocateAnswers404ForAnythingItCannotPlace(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"6b3e2c1a-0000-4000-8000-000000000000", "not-a-uuid"} {
		resp := h.get(t, "/v1/timeline/locate?id="+id)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("id %q gave %d, want 404", id, resp.StatusCode)
		}
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

// The viewer's panel draws a place name in the row that used to hold only
// coordinates, so the detail has to carry it — including for a photograph in
// the vault, whose panel is the same panel and whose place name lives in the
// sealed document rather than on the row.
func TestAssetDetailCarriesThePlaceName(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadNamedFixture(t, "iphone-portrait.heic"), map[string]string{
		"X-Photo-Filename": "iphone-portrait.heic",
	}))
	h.derive(t, up.ID)

	place := db.Place{City: "New York City", Admin1: "New York", Country: "United States", Source: "geonames"}
	if err := h.store.ApplyPlace(context.Background(), up.ID, place); err != nil {
		t.Fatalf("record a place: %v", err)
	}

	var got assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+up.ID), &got)

	if got.PlaceCity != place.City || got.PlaceAdmin1 != place.Admin1 || got.PlaceCountry != place.Country {
		t.Errorf("place = %q/%q/%q, want %q/%q/%q",
			got.PlaceCity, got.PlaceAdmin1, got.PlaceCountry, place.City, place.Admin1, place.Country)
	}
	// The coordinates stay beside it. The name is what the photograph is of;
	// the numbers are what the camera recorded.
	if got.GPSLat == nil || got.GPSLon == nil {
		t.Error("the detail dropped the coordinates the place was resolved from")
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
