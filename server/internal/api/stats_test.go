package api

import (
	"io"
	"net/http"
	"testing"
)

func getStats(t *testing.T, h *harness) statsResponse {
	t.Helper()
	resp := h.get(t, "/v1/stats")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/stats returned %d, want 200: %s", resp.StatusCode, body)
	}
	var out statsResponse
	decodeJSON(t, resp, &out)
	return out
}

func TestStatsCountAnUploadAgainstTheUploadingDevice(t *testing.T) {
	h := newHarness(t)

	before := getStats(t, h)
	if before.Device.Archived != 0 || before.Archive.Assets != 0 {
		t.Fatalf("a fresh archive reported %+v", before)
	}

	content := loadFixture(t)
	if resp := h.upload(t, content, nil); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload returned %d: %s", resp.StatusCode, body)
	}

	after := getStats(t, h)
	if after.Device.Archived != 1 {
		t.Errorf("device.archived = %d, want 1", after.Device.Archived)
	}
	if after.Device.Bytes != int64(len(content)) {
		t.Errorf("device.bytes = %d, want %d", after.Device.Bytes, len(content))
	}
	if after.Device.Photos != 1 || after.Device.Videos != 0 {
		t.Errorf("photos = %d, videos = %d; want 1, 0", after.Device.Photos, after.Device.Videos)
	}
	if after.Device.LastUploadAt == nil {
		t.Error("device.last_upload_at is null after an upload")
	}
	if after.Archive.Assets != 1 {
		t.Errorf("archive.assets = %d, want 1", after.Archive.Assets)
	}
	// The upload enqueues a metadata job in the same transaction, which is the
	// whole reason the app is told about the queue: the photo is safe and the
	// thumbnail is not built yet.
	if after.Archive.PendingJobs != 1 {
		t.Errorf("archive.pending_jobs = %d, want 1", after.Archive.PendingJobs)
	}
}

// The device block is the asking device's, not the archive's. A second phone
// sees an archive that holds something and a backup of its own that is empty.
func TestStatsAreScopedToTheTokenThatAsked(t *testing.T) {
	h := newHarness(t)
	if resp := h.upload(t, loadFixture(t), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload returned %d", resp.StatusCode)
	}

	_, secondToken := h.pair(t, "a second phone")
	h.token = secondToken

	stats := getStats(t, h)
	if stats.Device.Archived != 0 {
		t.Errorf("device.archived = %d for a phone that has sent nothing, want 0", stats.Device.Archived)
	}
	if stats.Archive.Assets != 1 {
		t.Errorf("archive.assets = %d, want 1; the archive is every device's", stats.Archive.Assets)
	}
}

func TestStatsRequireADeviceToken(t *testing.T) {
	h := newHarness(t)
	h.token = ""

	resp := h.get(t, "/v1/stats")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Not a write, but it is answered from a device token, and the plaintext
// listener refuses every one of those. 426 rather than 404 so the failure says
// what is actually wrong.
func TestStatsAreRefusedOnThePlaintextListener(t *testing.T) {
	h := newHarness(t)
	ts := h.plaintext(t)

	resp, err := ts.Client().Get(ts.URL + "/v1/stats")
	if err != nil {
		t.Fatalf("GET /v1/stats over plaintext: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("status = %d, want 426", resp.StatusCode)
	}
}
