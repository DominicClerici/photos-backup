package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// galleryUpload posts a body the way the browser page does: no token, no device,
// and a sha256 rather than an md5.
func (h *harness) galleryUpload(t *testing.T, server *httptest.Server, body []byte, overrides map[string]string) *http.Response {
	t.Helper()
	sum := sha256.Sum256(body)

	headers := map[string]string{
		"Content-Type":     "application/octet-stream",
		"X-Photo-Filename": "IMG_8071.HEIC",
		"X-Photo-Sha256":   hex.EncodeToString(sum[:]),
		"X-Photo-Size":     fmt.Sprint(len(body)),
	}
	for k, v := range overrides {
		if v == "" {
			delete(headers, k)
			continue
		}
		headers[k] = v
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/gallery/assets", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("gallery upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) galleryCheck(t *testing.T, server *httptest.Server, shas ...string) []contentCheckMatch {
	t.Helper()
	body, err := json.Marshal(contentCheckRequest{SHA256: shas})
	if err != nil {
		t.Fatalf("encode check: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/gallery/uploads/check", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("check returned %d: %s", resp.StatusCode, raw)
	}
	var out contentCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode check response: %v", err)
	}
	return out.Known
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// The whole point of the endpoint: a browser with no device token, on the
// listener that refuses every other write, can still put a photograph in.
func TestGalleryUploadNeedsNoToken(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)
	content := loadFixture(t)

	resp := h.galleryUpload(t, plain, content, nil)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("gallery upload returned %d: %s", resp.StatusCode, raw)
	}
	up := decodeUpload(t, resp)
	if up.ID == "" {
		t.Fatal("gallery upload returned no id")
	}
	if up.Duplicate {
		t.Error("duplicate = true on a first upload")
	}
	if up.SHA256 != digestOf(content) {
		t.Errorf("sha256 = %q, want %q", up.SHA256, digestOf(content))
	}

	asset, err := h.store.Asset(context.Background(), up.ID)
	if err != nil {
		t.Fatalf("load asset: %v", err)
	}
	if asset.DeviceID != galleryDeviceID {
		t.Errorf("device_id = %q, want %q", asset.DeviceID, galleryDeviceID)
	}
	if asset.LocalID != "" {
		t.Errorf("local_id = %q, want empty: the browser has nothing to sync", asset.LocalID)
	}

	// No mapping, because there is nothing on the other end to recognise later.
	var mappings int
	err = h.store.Pool().QueryRow(context.Background(),
		`select count(*) from device_assets where device_id = $1`, galleryDeviceID).Scan(&mappings)
	if err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if mappings != 0 {
		t.Errorf("gallery upload wrote %d device mappings, want 0", mappings)
	}
}

// Sending the same bytes twice archives them once and says so, which is what
// the page draws as a duplicate rather than a failure.
func TestGalleryUploadReportsDuplicate(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)
	content := loadFixture(t)

	first := decodeUpload(t, h.galleryUpload(t, plain, content, nil))
	second := decodeUpload(t, h.galleryUpload(t, plain, content, nil))

	if !second.Duplicate {
		t.Error("duplicate = false on re-upload of identical bytes")
	}
	if second.ID != first.ID {
		t.Errorf("re-upload returned id %q, want %q", second.ID, first.ID)
	}
	if blobs := h.blobFiles(t); len(blobs) != 1 {
		t.Errorf("blob tree holds %d files after re-upload, want 1: %v", len(blobs), blobs)
	}
}

// The digest is optional but binding. A page that declares one and sends
// something else gets nothing archived.
func TestGalleryUploadVerifiesDeclaredDigest(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)

	resp := h.galleryUpload(t, plain, loadFixture(t), map[string]string{
		"X-Photo-Sha256": strings.Repeat("a", 64),
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("mismatched sha256 returned %d, want 422", resp.StatusCode)
	}
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("rejected upload left blobs behind: %v", blobs)
	}
	if entries := h.manifestEntries(t); len(entries) != 0 {
		t.Errorf("rejected upload wrote %d manifest lines, want 0", len(entries))
	}
}

func TestGalleryUploadRejectsMalformedDigest(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)

	resp := h.galleryUpload(t, plain, loadFixture(t), map[string]string{"X-Photo-Sha256": "not-a-digest"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed sha256 returned %d, want 400", resp.StatusCode)
	}
}

// Without a digest the length still has to be right, which is what catches a
// body that was cut short.
func TestGalleryUploadWithoutDigestStillChecksLength(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)
	content := loadFixture(t)

	resp := h.galleryUpload(t, plain, content, map[string]string{"X-Photo-Sha256": ""})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload with no digest returned %d: %s", resp.StatusCode, raw)
	}

	short := h.galleryUpload(t, plain, content, map[string]string{
		"X-Photo-Sha256": "",
		"X-Photo-Size":   fmt.Sprint(len(content) + 1),
	})
	if short.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong declared size returned %d, want 422", short.StatusCode)
	}
}

// The rule this path has and the device path does not: whatever was dropped on
// a web page is not automatically a photograph.
func TestGalleryUploadRefusesUnrecognisedFile(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)

	resp := h.galleryUpload(t, plain, []byte("id,name\n1,Ada\n"), map[string]string{
		"X-Photo-Filename": "budget.csv",
		"X-Photo-Sha256":   digestOf([]byte("id,name\n1,Ada\n")),
		"X-Photo-Size":     fmt.Sprint(len("id,name\n1,Ada\n")),
	})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("csv upload returned %d, want 415", resp.StatusCode)
	}
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("refused upload left blobs behind: %v", blobs)
	}
}

// A photo with no extension whose bytes identify it is still a photo. This is
// the case that keeps the 415 above from being a filename whitelist.
func TestGalleryUploadAcceptsUnnamedButRecognisableFile(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)
	content := loadNamedFixture(t, "photo.jpg")

	resp := h.galleryUpload(t, plain, content, map[string]string{"X-Photo-Filename": "IMG_0001"})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("extensionless jpeg returned %d: %s", resp.StatusCode, raw)
	}
}

// The check is what turns a duplicate into a row on the page before any bytes
// move, so it has to answer for content in the library, in the trash, and for
// content the archive has never seen.
func TestGalleryCheckFindsArchivedContent(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)
	content := loadFixture(t)
	sha := digestOf(content)

	if known := h.galleryCheck(t, plain, sha); len(known) != 0 {
		t.Fatalf("check knew %v before anything was uploaded", known)
	}

	up := decodeUpload(t, h.galleryUpload(t, plain, content, nil))

	known := h.galleryCheck(t, plain, sha, strings.Repeat("b", 64))
	if len(known) != 1 {
		t.Fatalf("check returned %d matches, want 1: %v", len(known), known)
	}
	if known[0].Where != "library" {
		t.Errorf("where = %q, want library", known[0].Where)
	}
	if known[0].AssetID != up.ID {
		t.Errorf("id = %q, want %q", known[0].AssetID, up.ID)
	}
	if known[0].Filename != "IMG_8071.HEIC" {
		t.Errorf("filename = %q, want the archived name", known[0].Filename)
	}

	if resp := h.postJSON(t, "/v1/trash", fmt.Sprintf(`{"ids":[%q]}`, up.ID)); resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("trash returned %d: %s", resp.StatusCode, raw)
	}

	known = h.galleryCheck(t, plain, sha)
	if len(known) != 1 || known[0].Where != "trash" {
		t.Errorf("after a delete the check said %v, want one trash match", known)
	}
}

func TestGalleryCheckRejectsMalformedDigest(t *testing.T) {
	h := newHarness(t)
	plain := h.plaintext(t)

	req, err := http.NewRequest(http.MethodPost, plain.URL+"/v1/gallery/uploads/check",
		strings.NewReader(`{"sha256":["nope"]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := plain.Client().Do(req)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed digest returned %d, want 400", resp.StatusCode)
	}
}
