package api

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
)

// chunkedUpload drives the whole resumable protocol the way a client does.
type chunkedUpload struct {
	h        *harness
	body     []byte
	filename string
	localID  string
}

func newChunked(h *harness, body []byte) *chunkedUpload {
	return &chunkedUpload{
		h:        h,
		body:     body,
		filename: "IMG_8302.MOV",
		localID:  "B84E8479-475C-4727-A4A4-B77AA9980897/L0/001",
	}
}

func (c *chunkedUpload) md5() string {
	sum := md5.Sum(c.body)
	return hex.EncodeToString(sum[:])
}

// begin opens the session, and is also how a client resumes: the same call
// with the same declaration returns the same id and the current offset.
func (c *chunkedUpload) begin(t *testing.T) sessionResponse {
	t.Helper()
	payload, err := json.Marshal(createSessionRequest{
		DeviceID: c.h.deviceID,
		LocalID:  c.localID,
		Filename: c.filename,
		MD5:      c.md5(),
		Size:     int64(len(c.body)),
	})
	if err != nil {
		t.Fatalf("marshal declaration: %v", err)
	}

	resp := c.h.postJSON(t, "/v1/uploads", string(payload))
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create session: status %d: %s", resp.StatusCode, raw)
	}
	return decodeSession(t, resp)
}

func (c *chunkedUpload) put(t *testing.T, id string, start, end int) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		c.h.server.URL+"/v1/uploads/"+id, bytes.NewReader(c.body[start:end]))
	if err != nil {
		t.Fatalf("build chunk request: %v", err)
	}
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(c.body)))

	c.h.authorize(req)
	resp, err := c.h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT chunk: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (c *chunkedUpload) commit(t *testing.T, id string) *http.Response {
	t.Helper()
	return c.h.postJSON(t, "/v1/uploads/"+id+"/commit", "")
}

func decodeSession(t *testing.T, resp *http.Response) sessionResponse {
	t.Helper()
	var out sessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return out
}

// A chunked upload has to land exactly where a single-shot one does: same blob
// path, same manifest line, same row. The two paths share finishUpload for
// precisely this reason.
func TestChunkedUploadCommitsLikeASingleShotOne(t *testing.T) {
	h := newHarness(t)
	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)

	session := c.begin(t)
	if session.Offset != 0 {
		t.Fatalf("new session at offset %d, want 0", session.Offset)
	}

	const chunk = 4096
	for start := 0; start < len(content); start += chunk {
		end := min(start+chunk, len(content))
		resp := c.put(t, session.UploadID, start, end)
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("chunk at %d: status %d: %s", start, resp.StatusCode, raw)
		}
		if got := decodeSession(t, resp); got.Offset != int64(end) {
			t.Fatalf("offset after chunk = %d, want %d", got.Offset, end)
		}
	}

	resp := c.commit(t, session.UploadID)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("commit: status %d: %s", resp.StatusCode, raw)
	}
	got := decodeUpload(t, resp)

	want := sha256.Sum256(content)
	if got.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 = %q, want %q", got.SHA256, hex.EncodeToString(want[:]))
	}

	blobs := h.blobFiles(t)
	if len(blobs) != 1 {
		t.Fatalf("blob tree holds %d files, want 1: %v", len(blobs), blobs)
	}
	if want := filepath.Join(got.SHA256[0:2], got.SHA256[2:4], got.SHA256+".mov"); blobs[0] != want {
		t.Errorf("blob at %q, want %q", blobs[0], want)
	}

	entries := h.manifestEntries(t)
	if len(entries) != 1 {
		t.Fatalf("manifest holds %d lines, want 1", len(entries))
	}
	if entries[0].SHA256 != got.SHA256 || entries[0].Size != int64(len(content)) {
		t.Errorf("manifest entry wrong: %+v", entries[0])
	}

	// The staging directory is not a place bytes are allowed to accumulate.
	if left := stagedFiles(t, h); len(left) != 0 {
		t.Errorf("staging still holds %v", left)
	}
}

// The failure the whole feature exists for: a transfer dies partway through and
// the client comes back knowing nothing but the file it was sending.
func TestChunkedUploadResumesAfterAnInterruption(t *testing.T) {
	h := newHarness(t)
	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)

	first := c.begin(t)
	half := len(content) / 2
	if resp := c.put(t, first.UploadID, 0, half); resp.StatusCode != http.StatusOK {
		t.Fatalf("first chunk: status %d", resp.StatusCode)
	}

	// The client is killed here. All it can do on restart is re-declare.
	resumed := c.begin(t)
	if resumed.UploadID != first.UploadID {
		t.Errorf("resumed id = %q, want %q", resumed.UploadID, first.UploadID)
	}
	if resumed.Offset != int64(half) {
		t.Fatalf("resumed at offset %d, want %d", resumed.Offset, half)
	}

	if resp := c.put(t, resumed.UploadID, half, len(content)); resp.StatusCode != http.StatusOK {
		t.Fatalf("resumed chunk: status %d", resp.StatusCode)
	}

	resp := c.commit(t, resumed.UploadID)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("commit: status %d: %s", resp.StatusCode, raw)
	}

	want := sha256.Sum256(content)
	if got := decodeUpload(t, resp); got.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("resumed upload committed %q, want %q", got.SHA256, hex.EncodeToString(want[:]))
	}
}

// A client whose idea of progress has drifted gets told the truth rather than
// being made to start a 550MB video over.
func TestChunkAtWrongOffsetAnswersConflictWithTheOffset(t *testing.T) {
	h := newHarness(t)
	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)

	session := c.begin(t)
	if resp := c.put(t, session.UploadID, 0, 1000); resp.StatusCode != http.StatusOK {
		t.Fatalf("first chunk: status %d", resp.StatusCode)
	}

	resp := c.put(t, session.UploadID, 500, 1500)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := decodeSession(t, resp); got.Offset != 1000 {
		t.Errorf("conflict reported offset %d, want 1000", got.Offset)
	}
}

// Committing a session that is short of its declared size must not produce a
// blob: the md5 could not possibly match, and a partial original is worse than
// no original because it looks archived.
func TestCommitBeforeCompleteIsRefused(t *testing.T) {
	h := newHarness(t)
	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)

	session := c.begin(t)
	if resp := c.put(t, session.UploadID, 0, 100); resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}

	resp := c.commit(t, session.UploadID)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := decodeSession(t, resp); got.Offset != 100 {
		t.Errorf("offset = %d, want 100", got.Offset)
	}
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("an incomplete session produced blobs: %v", blobs)
	}
}

// Bytes that assemble into something other than what was declared are thrown
// away, and the session with them, so the retry starts clean.
func TestCommitRejectsAssembledBytesThatDoNotMatchTheDeclaration(t *testing.T) {
	h := newHarness(t)
	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)

	session := c.begin(t)

	// Declare one file, send another of the same length.
	corrupt := make([]byte, len(content))
	copy(corrupt, content)
	corrupt[len(corrupt)/2] ^= 0xFF
	c.body = corrupt

	if resp := c.put(t, session.UploadID, 0, len(corrupt)); resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}

	resp := c.commit(t, session.UploadID)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if blobs := h.blobFiles(t); len(blobs) != 0 {
		t.Errorf("a mismatched upload produced blobs: %v", blobs)
	}
	if left := stagedFiles(t, h); len(left) != 0 {
		t.Errorf("staging still holds %v after a rejected commit", left)
	}
	if entries := h.manifestEntries(t); len(entries) != 0 {
		t.Errorf("manifest gained %d lines from a rejected commit", len(entries))
	}
}

// Re-sending bytes the archive already holds costs a session and no blob, which
// is what makes a retried upload free rather than duplicated.
func TestCommittingAlreadyArchivedBytesIsADuplicate(t *testing.T) {
	h := newHarness(t)
	content := loadNamedFixture(t, "clip.mov")

	single := decodeUpload(t, h.upload(t, content, map[string]string{
		"X-Photo-Filename": "IMG_8302.MOV",
	}))

	c := newChunked(h, content)
	c.localID = "a-different-local-id"
	session := c.begin(t)
	if resp := c.put(t, session.UploadID, 0, len(content)); resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}

	got := decodeUpload(t, c.commit(t, session.UploadID))
	if !got.Duplicate {
		t.Error("duplicate = false for bytes already archived")
	}
	if got.ID != single.ID {
		t.Errorf("id = %q, want %q", got.ID, single.ID)
	}
	if blobs := h.blobFiles(t); len(blobs) != 1 {
		t.Errorf("blob tree holds %d files, want 1: %v", len(blobs), blobs)
	}
}

func TestUnknownSessionIsNotFound(t *testing.T) {
	h := newHarness(t)
	const absent = "0123456789abcdef0123456789abcdef"

	if resp := h.raw(t, http.MethodGet, "/v1/uploads/"+absent, h.token); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", resp.StatusCode)
	}
	if resp := h.postJSON(t, "/v1/uploads/"+absent+"/commit", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("commit status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateSessionRejectsAnIncompleteDeclaration(t *testing.T) {
	h := newHarness(t)

	// No deviceId: the token supplies it. What is missing is the md5 and size.
	resp := h.postJSON(t, "/v1/uploads", `{"localId":"x","filename":"a.mov"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestChunkNeedsAnOffsetHeader(t *testing.T) {
	h := newHarness(t)
	c := newChunked(h, loadNamedFixture(t, "clip.mov"))
	session := c.begin(t)

	req, err := http.NewRequest(http.MethodPut,
		h.server.URL+"/v1/uploads/"+session.UploadID, bytes.NewReader([]byte("bytes")))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	h.authorize(req)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAbortDiscardsTheSession(t *testing.T) {
	h := newHarness(t)
	c := newChunked(h, loadNamedFixture(t, "clip.mov"))
	session := c.begin(t)
	if resp := c.put(t, session.UploadID, 0, 500); resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodDelete, h.server.URL+"/v1/uploads/"+session.UploadID, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	h.authorize(req)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if left := stagedFiles(t, h); len(left) != 0 {
		t.Errorf("staging still holds %v after abort", left)
	}
}

func stagedFiles(t *testing.T, h *harness) []string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(h.photosRoot, "incoming", "*"))
	if err != nil {
		t.Fatalf("glob staging: %v", err)
	}
	return entries
}
