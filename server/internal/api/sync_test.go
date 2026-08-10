package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

func (h *harness) check(t *testing.T, req syncCheckRequest) *http.Response {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal check request: %v", err)
	}
	resp, err := h.server.Client().Post(h.server.URL+"/v1/sync/check", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post sync check: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// checkOK posts a request, insists on a 200, and returns the results keyed by
// local id. Order-sensitive tests decode the response themselves.
func (h *harness) checkOK(t *testing.T, req syncCheckRequest) map[string]syncCheckResult {
	t.Helper()
	resp := h.check(t, req)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("sync check status = %d, want 200: %s", resp.StatusCode, body)
	}
	var decoded syncCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode check response: %v", err)
	}
	byLocalID := make(map[string]syncCheckResult, len(decoded.Results))
	for _, r := range decoded.Results {
		byLocalID[r.LocalID] = r
	}
	return byLocalID
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func size(n int) *int64 {
	v := int64(n)
	return &v
}

const testDevice = "iphone-14-pro"

// Round one carries no digest, so an unrecognised local id can only be reported
// as unknown. This is what stops the phone hashing its whole library up front.
func TestSyncCheckReportsUnknownWithoutADigest(t *testing.T) {
	h := newHarness(t)

	got := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: "never-seen"}},
	})

	if got["never-seen"].Status != statusUnknown {
		t.Errorf("status = %q, want %q", got["never-seen"].Status, statusUnknown)
	}
}

func TestSyncCheckWantsContentThatIsNotArchived(t *testing.T) {
	h := newHarness(t)

	got := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items: []syncCheckItem{{
			LocalID: "never-seen",
			MD5:     md5Hex([]byte("not in the archive")),
			Size:    size(18),
		}},
	})

	if got["never-seen"].Status != statusWant {
		t.Errorf("status = %q, want %q", got["never-seen"].Status, statusWant)
	}
}

// The second-run criterion: after an upload, round one alone answers have, with
// no digest supplied and therefore no hashing on the phone.
func TestSyncCheckReportsHaveByLocalIDAfterUpload(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	got := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"}},
	})

	result := got["B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"]
	if result.Status != statusHave {
		t.Errorf("status = %q, want %q", result.Status, statusHave)
	}
	if result.AssetID != up.ID {
		t.Errorf("assetId = %q, want %q", result.AssetID, up.ID)
	}
}

// A second copy of the same photo in the library has a different local id. It
// must resolve by content, and the mapping learned from that must make round one
// sufficient next time.
func TestSyncCheckLearnsMappingFromContentMatch(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	up := decodeUpload(t, h.upload(t, content, nil))

	byContent := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items: []syncCheckItem{{
			LocalID: "second-copy-of-the-same-photo",
			MD5:     md5Hex(content),
			Size:    size(len(content)),
		}},
	})
	if byContent["second-copy-of-the-same-photo"].Status != statusHave {
		t.Fatalf("content match status = %q, want %q", byContent["second-copy-of-the-same-photo"].Status, statusHave)
	}
	if byContent["second-copy-of-the-same-photo"].AssetID != up.ID {
		t.Errorf("assetId = %q, want the archived asset %q", byContent["second-copy-of-the-same-photo"].AssetID, up.ID)
	}

	byLocalID := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: "second-copy-of-the-same-photo"}},
	})
	if byLocalID["second-copy-of-the-same-photo"].Status != statusHave {
		t.Errorf("round one after learning = %q, want %q; the mapping was not recorded",
			byLocalID["second-copy-of-the-same-photo"].Status, statusHave)
	}
}

// An md5 and size matching two different assets identify nothing. Claiming have
// would mean the server had guessed which bytes the phone holds.
func TestSyncCheckWantsAmbiguousContent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Planted through the store: the upload path derives md5 from real bytes, so
	// a collision cannot be staged over HTTP.
	base := db.Asset{
		MD5:              "d41d8cd98f00b204e9800998ecf8427e",
		ByteSize:         989,
		OriginalFilename: "IMG_0001.HEIC",
		Ext:              ".heic",
		ContentType:      "image/heic",
		DeviceID:         testDevice,
	}
	for i, sha := range []string{
		"1111111111111111111111111111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222222222222222222222222222",
	} {
		a := base
		a.SHA256 = sha
		a.LocalID = fmt.Sprintf("planted-%d", i)
		if _, _, err := h.store.RecordAsset(ctx, a); err != nil {
			t.Fatalf("plant asset %d: %v", i, err)
		}
	}

	got := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: "a-third-local-id", MD5: base.MD5, Size: size(989)}},
	})

	if got["a-third-local-id"].Status != statusWant {
		t.Errorf("status = %q, want %q for ambiguous content", got["a-third-local-id"].Status, statusWant)
	}
}

// Editing a photo keeps its local identifier and changes its bytes, so a stale
// modification time must send the item back through a content check.
func TestSyncCheckReportsUnknownAfterModificationTimeChanges(t *testing.T) {
	h := newHarness(t)
	modified := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	h.upload(t, loadFixture(t), map[string]string{
		"X-Photo-Modified-At": modified.Format(time.RFC3339),
	})
	localID := "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"

	unchanged := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: localID, ModifiedAt: &modified}},
	})
	if unchanged[localID].Status != statusHave {
		t.Errorf("unchanged asset status = %q, want %q", unchanged[localID].Status, statusHave)
	}

	edited := modified.Add(time.Hour)
	after := h.checkOK(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: localID, ModifiedAt: &edited}},
	})
	if after[localID].Status != statusUnknown {
		t.Errorf("edited asset status = %q, want %q so its new bytes get checked", after[localID].Status, statusUnknown)
	}
}

func TestSyncCheckPreservesRequestOrder(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	h.upload(t, content, nil)

	items := []syncCheckItem{
		{LocalID: "unknown-one"},
		{LocalID: "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001"},
		{LocalID: "wanted-one", MD5: md5Hex([]byte("absent")), Size: size(6)},
	}
	resp := h.check(t, syncCheckRequest{DeviceID: testDevice, Items: items})
	var decoded syncCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(decoded.Results) != len(items) {
		t.Fatalf("got %d results, want %d", len(decoded.Results), len(items))
	}
	wantStatus := []string{statusUnknown, statusHave, statusWant}
	for i, want := range wantStatus {
		if decoded.Results[i].LocalID != items[i].LocalID {
			t.Errorf("results[%d].localId = %q, want %q", i, decoded.Results[i].LocalID, items[i].LocalID)
		}
		if decoded.Results[i].Status != want {
			t.Errorf("results[%d].status = %q, want %q", i, decoded.Results[i].Status, want)
		}
	}
}

// Postgres refuses to let one statement's ON CONFLICT DO UPDATE touch a row
// twice, so a batch that repeats a local id must be deduplicated before it
// reaches the mapping upsert.
func TestSyncCheckToleratesARepeatedLocalID(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	h.upload(t, content, nil)

	item := syncCheckItem{LocalID: "repeated", MD5: md5Hex(content), Size: size(len(content))}
	resp := h.check(t, syncCheckRequest{DeviceID: testDevice, Items: []syncCheckItem{item, item}})

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var decoded syncCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(decoded.Results))
	}
	for i, r := range decoded.Results {
		if r.Status != statusHave {
			t.Errorf("results[%d].status = %q, want %q", i, r.Status, statusHave)
		}
	}
}

func TestSyncCheckAcceptsAnEmptyBatch(t *testing.T) {
	h := newHarness(t)

	resp := h.check(t, syncCheckRequest{DeviceID: testDevice, Items: nil})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestSyncCheckRejectsOversizedBatch(t *testing.T) {
	h := newHarness(t)
	items := make([]syncCheckItem, maxCheckItems+1)
	for i := range items {
		items[i] = syncCheckItem{LocalID: fmt.Sprintf("local-%d", i)}
	}

	resp := h.check(t, syncCheckRequest{DeviceID: testDevice, Items: items})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSyncCheckRejectsMissingDeviceID(t *testing.T) {
	h := newHarness(t)

	resp := h.check(t, syncCheckRequest{Items: []syncCheckItem{{LocalID: "x"}}})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSyncCheckRejectsMissingLocalID(t *testing.T) {
	h := newHarness(t)

	resp := h.check(t, syncCheckRequest{DeviceID: testDevice, Items: []syncCheckItem{{LocalID: "  "}}})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// A digest with no length would be answered "unknown" for an item the phone has
// already hashed, which is a loop. Reject it at the door instead.
func TestSyncCheckRejectsDigestWithoutSize(t *testing.T) {
	h := newHarness(t)

	resp := h.check(t, syncCheckRequest{
		DeviceID: testDevice,
		Items:    []syncCheckItem{{LocalID: "x", MD5: md5Hex([]byte("hi"))}},
	})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSyncCheckRejectsMalformedBody(t *testing.T) {
	h := newHarness(t)

	resp, err := h.server.Client().Post(h.server.URL+"/v1/sync/check", "application/json", bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// A check that cannot reach Postgres must not answer "have" by omission: the
// phone has to be told to try again, not told the archive is complete.
func TestSyncCheckWithDatabaseDownReportsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.store.Close()

	resp := h.check(t, syncCheckRequest{DeviceID: testDevice, Items: []syncCheckItem{{LocalID: "x"}}})

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
