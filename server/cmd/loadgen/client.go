package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// client speaks the same protocol the phone does, and nothing else. Anything it
// can do the app can do; anything it cannot, the app should not be relying on.
type client struct {
	base string
	http *http.Client
	// token is a device token from `photobackup pair`, exactly as the phone
	// holds one. It is not a test affordance: the write path has no
	// unauthenticated entrance, and giving this client a private one would make
	// it stop being a stand-in for the app.
	token string
}

// post issues a JSON POST carrying the device token.
func (c *client) post(path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	return c.http.Do(req)
}

func (c *client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
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

func (c *client) check(deviceID string, items []checkItem) ([]checkResult, error) {
	payload, err := json.Marshal(map[string]any{"deviceId": deviceID, "items": items})
	if err != nil {
		return nil, err
	}

	resp, err := c.post("/v1/sync/check", payload)
	if err != nil {
		return nil, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, statusError("sync/check", resp)
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

// outcome is what one transfer cost, as opposed to what the file weighs. A
// resumed upload sends less than its own size, and a harness that reported
// otherwise would overstate throughput on exactly the runs worth measuring.
type outcome struct {
	result  uploadResult
	sent    int64
	resumed bool
}

// uploadSingle is the small-file path: one request, body streamed from disk.
func (c *client) uploadSingle(deviceID string, item *item) (outcome, error) {
	f, err := os.Open(item.path)
	if err != nil {
		return outcome{}, err
	}
	defer f.Close()

	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/assets", f)
	if err != nil {
		return outcome{}, err
	}
	req.ContentLength = item.size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Photo-Filename", item.filename)
	req.Header.Set("X-Photo-Md5", item.md5)
	req.Header.Set("X-Photo-Size", fmt.Sprint(item.size))
	req.Header.Set("X-Photo-Device-Id", deviceID)
	req.Header.Set("X-Photo-Local-Id", item.localID)
	c.authorize(req)
	if item.capturedAt != nil {
		req.Header.Set("X-Photo-Captured-At", item.capturedAt.UTC().Format(time.RFC3339Nano))
	}
	if item.modifiedAt != nil {
		req.Header.Set("X-Photo-Modified-At", item.modifiedAt.UTC().Format(time.RFC3339Nano))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return outcome{}, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusCreated {
		return outcome{}, statusError("upload", resp)
	}
	var out uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return outcome{}, fmt.Errorf("decode upload response: %w", err)
	}
	return outcome{result: out, sent: item.size}, nil
}

type session struct {
	UploadID string `json:"uploadId"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	Complete bool   `json:"complete"`
}

// errAborted is what --abort-chunked-after raises. The partial upload is left on
// the server on purpose: the next run is supposed to find it and resume.
var errAborted = errors.New("aborted mid-upload on purpose")

// uploadChunked is the large-file path. It opens a session, seeks to whatever
// the server already holds, and sends the rest.
//
// The session id is derived from the declaration, so beginSession doubles as
// "where did I get to" — this function never has to remember anything between
// runs, which is the same property the phone depends on after being killed.
func (c *client) uploadChunked(deviceID string, item *item, chunkSize int64, abortAfter int) (outcome, error) {
	s, err := c.beginSession(deviceID, item)
	if err != nil {
		return outcome{}, err
	}
	// Anything already on the server is bytes this run does not have to send,
	// which is the entire point of the resumable path.
	out := outcome{resumed: s.Offset > 0}

	f, err := os.Open(item.path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	chunks := 0
	for s.Offset < item.size {
		if abortAfter > 0 && chunks == abortAfter {
			return out, errAborted
		}

		end := min(s.Offset+chunkSize, item.size)
		before := s.Offset
		s, err = c.putChunk(s, f, s.Offset, end, item.size)
		if err != nil {
			return out, err
		}
		out.sent += s.Offset - before
		chunks++
	}

	out.result, err = c.commitSession(s.UploadID)
	return out, err
}

func (c *client) beginSession(deviceID string, item *item) (session, error) {
	payload, err := json.Marshal(map[string]any{
		"deviceId":   deviceID,
		"localId":    item.localID,
		"filename":   item.filename,
		"md5":        item.md5,
		"size":       item.size,
		"capturedAt": item.capturedAt,
		"modifiedAt": item.modifiedAt,
	})
	if err != nil {
		return session{}, err
	}

	resp, err := c.post("/v1/uploads", payload)
	if err != nil {
		return session{}, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return session{}, statusError("create upload session", resp)
	}
	var s session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return session{}, fmt.Errorf("decode session: %w", err)
	}
	return s, nil
}

// putChunk sends one range. A 409 is not a failure: the server is reporting the
// offset it actually holds, and the loop picks up from there.
func (c *client) putChunk(s session, f *os.File, start, end, total int64) (session, error) {
	req, err := http.NewRequest(http.MethodPut, c.base+"/v1/uploads/"+s.UploadID,
		io.NewSectionReader(f, start, end-start))
	if err != nil {
		return s, err
	}
	req.ContentLength = end - start
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, total))
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return s, err
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusConflict:
		var next session
		if err := json.NewDecoder(resp.Body).Decode(&next); err != nil {
			return s, fmt.Errorf("decode chunk response: %w", err)
		}
		if next.Offset == s.Offset && resp.StatusCode == http.StatusConflict {
			return s, fmt.Errorf("chunk refused and offset did not move from %d", s.Offset)
		}
		return next, nil
	default:
		return s, statusError("chunk", resp)
	}
}

func (c *client) commitSession(id string) (uploadResult, error) {
	resp, err := c.post("/v1/uploads/"+id+"/commit", nil)
	if err != nil {
		return uploadResult{}, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusCreated {
		return uploadResult{}, statusError("commit", resp)
	}
	var out uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uploadResult{}, fmt.Errorf("decode commit response: %w", err)
	}
	return out, nil
}

func statusError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return fmt.Errorf("%s returned %d: %s", what, resp.StatusCode, bytes.TrimSpace(body))
}

// drain finishes the body so the connection returns to the pool. Without it a
// run of 3,000 uploads opens 3,000 sockets.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}
