package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"golang.org/x/image/webp"
)

func decodeUpload(t *testing.T, resp *http.Response) uploadResponse {
	t.Helper()
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return out
}

func TestUploadStoresBlobManifestLineAndRow(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	want := sha256.Sum256(content)

	resp := h.upload(t, content, nil)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}

	got := decodeUpload(t, resp)
	if got.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 = %q, want %q", got.SHA256, hex.EncodeToString(want[:]))
	}
	if got.Duplicate {
		t.Error("duplicate = true on a first upload")
	}
	if got.ID == "" {
		t.Error("response carried no asset id")
	}

	blobs := h.blobFiles(t)
	if len(blobs) != 1 {
		t.Fatalf("blob tree holds %d files, want 1: %v", len(blobs), blobs)
	}
	if want := filepath.Join(got.SHA256[0:2], got.SHA256[2:4], got.SHA256+".heic"); blobs[0] != want {
		t.Errorf("blob at %q, want %q", blobs[0], want)
	}

	entries := h.manifestEntries(t)
	if len(entries) != 1 {
		t.Fatalf("manifest holds %d lines, want 1", len(entries))
	}
	if entries[0].SHA256 != got.SHA256 || entries[0].Filename != "IMG_8071.HEIC" {
		t.Errorf("manifest entry wrong: %+v", entries[0])
	}
	if entries[0].Size != int64(len(content)) {
		t.Errorf("manifest size = %d, want %d", entries[0].Size, len(content))
	}
}

func TestUploadingSameBytesTwiceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)

	first := decodeUpload(t, h.upload(t, content, nil))
	second := decodeUpload(t, h.upload(t, content, map[string]string{
		"X-Photo-Local-Id": "a-different-local-id",
	}))

	if !second.Duplicate {
		t.Error("duplicate = false on re-upload of identical bytes")
	}
	if second.ID != first.ID {
		t.Errorf("re-upload returned id %q, want %q", second.ID, first.ID)
	}
	if blobs := h.blobFiles(t); len(blobs) != 1 {
		t.Errorf("blob tree holds %d files after re-upload, want 1: %v", len(blobs), blobs)
	}
	if entries := h.manifestEntries(t); len(entries) != 1 {
		t.Errorf("manifest holds %d lines after re-upload, want 1", len(entries))
	}
}

func TestUploadRejectsMD5MismatchAndWritesNothing(t *testing.T) {
	h := newHarness(t)

	resp := h.upload(t, loadFixture(t), map[string]string{
		"X-Photo-Md5": "00000000000000000000000000000000",
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("rejected upload left blobs behind: %v", blobs)
	}
	if entries := h.manifestEntries(t); len(entries) != 0 {
		t.Errorf("rejected upload wrote %d manifest lines, want 0", len(entries))
	}
	if resp := h.get(t, "/"); resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if bytes.Contains(body, []byte("/original")) {
			t.Error("rejected upload is visible in the gallery")
		}
	}
}

func TestUploadRejectsMissingRequiredHeader(t *testing.T) {
	h := newHarness(t)

	resp := h.upload(t, loadFixture(t), map[string]string{"X-Photo-Md5": ""})

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when X-Photo-Md5 is absent", resp.StatusCode)
	}
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("rejected upload left blobs behind: %v", blobs)
	}
}

func TestGetOriginalReturnsExactBytes(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	up := decodeUpload(t, h.upload(t, content, nil))

	resp := h.get(t, "/v1/assets/"+up.ID+"/original")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/heic" {
		t.Errorf("Content-Type = %q, want image/heic", ct)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("served %d bytes, want the original %d bytes", len(got), len(content))
	}
}

func TestGetWebReturnsDecodableWebP(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.get(t, "/v1/assets/"+up.ID+"/web")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/webp" {
		t.Errorf("Content-Type = %q, want image/webp", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if _, err := webp.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("response is not decodable WebP: %v", err)
	}
}

func TestGetUnknownAssetReturnsNotFound(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/v1/assets/6b3e2c1a-0000-4000-8000-000000000000/original")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGalleryShowsUploadedAsset(t *testing.T) {
	h := newHarness(t)
	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	resp := h.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte("/v1/assets/"+up.ID+"/web")) {
		t.Errorf("gallery does not link the uploaded asset\n%s", body)
	}
	if !bytes.Contains(body, []byte("IMG_8071.HEIC")) {
		t.Error("gallery does not show the original filename")
	}
}

// The archive must outlive the index: if Postgres is gone the bytes are still
// committed to disk, and the phone is told to retry rather than told success.
func TestUploadWithDatabaseDownKeepsBlobAndReportsUnavailable(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	h.store.Close()

	resp := h.upload(t, content, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if blobs := h.blobFiles(t); len(blobs) != 1 {
		t.Errorf("blob tree holds %d files, want the upload to be durable: %v", len(blobs), blobs)
	}
	if entries := h.manifestEntries(t); len(entries) != 1 {
		t.Errorf("manifest holds %d lines, want 1 so the row is recoverable", len(entries))
	}
}

func TestHealthReportsOK(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/health")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
