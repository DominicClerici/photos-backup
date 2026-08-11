package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// importClient speaks the same protocol the phone does.
//
// Over HTTP to photod rather than writing blobs and rows directly, even though
// this command runs on the archive machine and could reach both. Going through
// the API means an import commits in exactly the order an upload does — blob,
// manifest line, database row — instead of through a second implementation of
// that ordering that would have to be kept honest by inspection. It also means
// two processes are never appending to manifest.jsonl at once, which is the
// concrete way the direct path goes wrong.
type importClient struct {
	base   string
	token  string
	http   *http.Client
	device string
}

func (c *importClient) request(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

func (c *importClient) postJSON(path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := c.request(http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

type checkItem struct {
	LocalID    string     `json:"localId"`
	MD5        string     `json:"md5,omitempty"`
	Size       *int64     `json:"size,omitempty"`
	ModifiedAt *time.Time `json:"modifiedAt,omitempty"`
}

type checkResult struct {
	LocalID string `json:"localId"`
	Status  string `json:"status"`
	AssetID string `json:"assetId"`
}

// check asks which of these the archive already holds. Without it, re-running
// an import after a failure would re-send every byte that already landed.
func (c *importClient) check(items []checkItem) ([]checkResult, error) {
	resp, err := c.postJSON("/v1/sync/check", map[string]any{
		"deviceId": c.device, "items": items,
	})
	if err != nil {
		return nil, err
	}
	defer drainBody(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("sync/check", resp)
	}
	var out struct {
		Results []checkResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode sync/check response: %w", err)
	}
	return out.Results, nil
}

type uploadResult struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	Duplicate bool   `json:"duplicate"`
}

// upload sends one file in a single request.
//
// The content id header is what lets the pairing resolve in the same
// transaction that inserts the row, rather than waiting on the metadata job to
// read it back off the blob. Both routes reach the same place; this one avoids
// building an ordinary video's poster and transcode for a file that turns out
// to be three seconds of a Live Photo.
func (c *importClient) upload(it *importItem) (uploadResult, error) {
	f, err := os.Open(it.path)
	if err != nil {
		return uploadResult{}, err
	}
	defer f.Close()

	req, err := c.request(http.MethodPost, "/v1/assets", f)
	if err != nil {
		return uploadResult{}, err
	}
	req.ContentLength = it.size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Photo-Filename", it.filename)
	req.Header.Set("X-Photo-Md5", it.md5)
	req.Header.Set("X-Photo-Size", fmt.Sprint(it.size))
	req.Header.Set("X-Photo-Device-Id", c.device)
	req.Header.Set("X-Photo-Local-Id", it.localID)
	req.Header.Set("X-Photo-Modified-At", it.modified.Format(time.RFC3339Nano))
	if it.contentID != "" {
		req.Header.Set("X-Photo-Content-Id", it.contentID)
	}
	// The sidecar's capture time, which for anything whose EXIF survived is
	// redundant and for a screenshot is the only one there is. The metadata
	// worker's reading of the file still wins wherever the file has an opinion.
	if it.takenAt != nil {
		req.Header.Set("X-Photo-Captured-At", it.takenAt.UTC().Format(time.RFC3339Nano))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return uploadResult{}, err
	}
	defer drainBody(resp)

	if resp.StatusCode != http.StatusCreated {
		return uploadResult{}, statusErr("upload", resp)
	}
	var out uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uploadResult{}, fmt.Errorf("decode upload response: %w", err)
	}
	return out, nil
}

type session struct {
	UploadID string `json:"uploadId"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	Complete bool   `json:"complete"`
}

// uploadChunked is the large-video path. The session id is derived from the
// declaration, so opening one is also how a run that died halfway through a 4GB
// file finds out where it got to.
func (c *importClient) uploadChunked(it *importItem, chunkSize int64) (uploadResult, error) {
	resp, err := c.postJSON("/v1/uploads", map[string]any{
		"deviceId":   c.device,
		"localId":    it.localID,
		"filename":   it.filename,
		"md5":        it.md5,
		"size":       it.size,
		"capturedAt": it.takenAt,
		"modifiedAt": it.modified,
		"contentId":  it.contentID,
	})
	if err != nil {
		return uploadResult{}, err
	}
	var s session
	err = decodeInto(resp, "create upload session", http.StatusOK, &s)
	if err != nil {
		return uploadResult{}, err
	}

	f, err := os.Open(it.path)
	if err != nil {
		return uploadResult{}, err
	}
	defer f.Close()

	for s.Offset < it.size {
		end := min(s.Offset+chunkSize, it.size)
		req, err := c.request(http.MethodPut, "/v1/uploads/"+s.UploadID,
			io.NewSectionReader(f, s.Offset, end-s.Offset))
		if err != nil {
			return uploadResult{}, err
		}
		req.ContentLength = end - s.Offset
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s.Offset, end-1, it.size))

		resp, err := c.http.Do(req)
		if err != nil {
			return uploadResult{}, err
		}
		before := s.Offset
		// A 409 is not a failure: the server is reporting the offset it really
		// holds, and the loop picks up from there.
		if err := decodeInto(resp, "chunk", http.StatusOK, &s, http.StatusConflict); err != nil {
			return uploadResult{}, err
		}
		if s.Offset <= before {
			return uploadResult{}, fmt.Errorf("upload of %s stalled at offset %d", it.filename, s.Offset)
		}
	}

	resp, err = c.postJSON("/v1/uploads/"+s.UploadID+"/commit", nil)
	if err != nil {
		return uploadResult{}, err
	}
	var out uploadResult
	return out, decodeInto(resp, "commit", http.StatusCreated, &out)
}

type albumPayload struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// describe hands the archive the sidecar, after the bytes are safely in it.
//
// Deliberately a second request rather than more upload headers. A sidecar is
// JSON of unbounded length holding names, captions, and coordinates, none of
// which belongs in a header, and separating them means a metadata failure costs
// the metadata rather than the photo.
func (c *importClient) describe(assetID string, it *importItem) error {
	albums := make([]albumPayload, 0, len(it.albums))
	for _, album := range it.albums {
		albums = append(albums, albumPayload{Title: album.Title, Description: album.Description})
	}

	resp, err := c.postJSON("/v1/assets/"+assetID+"/import-metadata", map[string]any{
		"source":  "google-takeout",
		"sidecar": it.sidecar,
		"albums":  albums,
	})
	if err != nil {
		return err
	}
	defer drainBody(resp)

	if resp.StatusCode != http.StatusNoContent {
		return statusErr("import-metadata", resp)
	}
	return nil
}

// decodeInto reads a response body into v, treating any of the accepted status
// codes as success. It always drains and closes.
func decodeInto(resp *http.Response, what string, want int, v any, alsoOK ...int) error {
	defer drainBody(resp)

	ok := resp.StatusCode == want
	for _, code := range alsoOK {
		ok = ok || resp.StatusCode == code
	}
	if !ok {
		return statusErr(what, resp)
	}
	if v == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode %s response: %w", what, err)
	}
	return nil
}

func statusErr(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return fmt.Errorf("%s returned %d: %s", what, resp.StatusCode, bytes.TrimSpace(body))
}

// drainBody finishes the body so the connection returns to the pool. Without it
// an import of 50,000 files opens 50,000 sockets.
func drainBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}
