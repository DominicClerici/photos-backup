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

	"github.com/dominicclerici/photos-backup/server/internal/db"
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

	var page db.TimelinePage
	decodeJSON(t, h.get(t, "/v1/timeline"), &page)
	if len(page.Items) != 0 {
		t.Errorf("rejected upload is on the timeline: %+v", page.Items)
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

func TestGetUnknownAssetReturnsNotFound(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/v1/assets/6b3e2c1a-0000-4000-8000-000000000000/original")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// A malformed id reaches Postgres as a failed uuid cast, which must not be
// reported as the database being down.
func TestGetMalformedAssetIDReturnsNotFound(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/v1/assets/not-a-uuid/original")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// With Postgres gone the write path closes, because authenticating a device
// reads it.
//
// This is a deliberate change from Phase 4, where the same request stored its
// blob and then failed to index it. Nothing is lost either way — the phone never
// gets an ack, so the item stays queued and the bytes arrive when the database
// does — and the alternative is caching tokens in memory, which would buy an
// early blob write at the cost of a revoked device still being able to upload
// for as long as the cache held it. Refusing is the cheaper answer.
func TestUploadWithDatabaseDownIsRefused(t *testing.T) {
	h := newHarness(t)
	content := loadFixture(t)
	h.store.Close()

	resp := h.upload(t, content, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// And it fails before touching the disk, so there is no half-committed
	// upload to reconcile later.
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("blob tree holds %v, want nothing: the request never got past authentication", blobs)
	}
	if entries := h.manifestEntries(t); len(entries) != 0 {
		t.Errorf("manifest holds %d lines, want none", len(entries))
	}
}

func TestHealthReportsOK(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/health")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// The upload path must not wait on derivative work, and must not forget to ask
// for it either.
func TestUploadWakesTheWorkerWithoutWaitingForIt(t *testing.T) {
	h := newHarness(t)

	up := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	if h.nudges.Load() != 1 {
		t.Errorf("nudges = %d, want 1", h.nudges.Load())
	}

	// The response arrives with the asset still underived.
	var detail assetDetail
	decodeJSON(t, h.get(t, "/v1/assets/"+up.ID), &detail)
	if detail.State != db.DerivedPending {
		t.Errorf("state = %q immediately after upload, want pending", detail.State)
	}

	// A duplicate has nothing new to derive, so it must not wake anyone.
	h.upload(t, loadFixture(t), map[string]string{"X-Photo-Local-Id": "another-local-id"})
	if h.nudges.Load() != 1 {
		t.Errorf("nudges = %d after a duplicate upload, want 1", h.nudges.Load())
	}
}

func decodeJSON(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
