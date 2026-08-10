package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

type uploadResponse struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	Duplicate bool   `json:"duplicate"`
}

// handleUpload commits an original in the order that makes a crash survivable:
// blob first, then the manifest line, then the database row. Anything lost
// after the blob lands is recoverable by re-uploading the same bytes.
//
// Derivative work is queued inside the same transaction as the row, and this
// handler returns without waiting for any of it. Upload throughput is never
// behind ffmpeg.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	meta, err := parseUploadHeaders(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ext := strings.ToLower(filepath.Ext(meta.filename))
	res, err := s.Blobs.Put(r.Body, ext, blobstore.Expected{MD5: meta.md5, Size: meta.size})
	switch {
	case errors.Is(err, blobstore.ErrChecksumMismatch), errors.Is(err, blobstore.ErrSizeMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.logger().Error("store blob", "error", err)
		writeError(w, http.StatusInternalServerError, "could not store upload")
		return
	}

	if res.Created {
		entry := manifest.Entry{
			SHA256:     res.SHA256,
			MD5:        res.MD5,
			Size:       res.Size,
			Filename:   meta.filename,
			CapturedAt: meta.capturedAt,
			ModifiedAt: meta.modifiedAt,
			DeviceID:   meta.deviceID,
			LocalID:    meta.localID,
			StoredAt:   time.Now().UTC(),
		}
		if err := s.Manifest.Append(entry); err != nil {
			s.logger().Error("append manifest", "error", err, "sha256", res.SHA256)
			writeError(w, http.StatusInternalServerError, "could not record upload")
			return
		}
	}

	contentType := contentTypeFor(ext)
	id, inserted, err := s.Store.RecordAsset(r.Context(), db.Asset{
		SHA256:           res.SHA256,
		MD5:              res.MD5,
		ByteSize:         res.Size,
		OriginalFilename: meta.filename,
		Ext:              ext,
		ContentType:      contentType,
		MediaKind:        mediaKindFor(contentType),
		CapturedAt:       meta.capturedAt,
		ModifiedAt:       meta.modifiedAt,
		DeviceID:         meta.deviceID,
		LocalID:          meta.localID,
	})
	if err != nil {
		s.logger().Error("record asset", "error", err, "sha256", res.SHA256)
		writeError(w, http.StatusServiceUnavailable, "upload stored but not indexed; retry to reconcile")
		return
	}

	if inserted {
		s.nudge()
	}

	writeJSON(w, http.StatusCreated, uploadResponse{
		ID:        id,
		SHA256:    res.SHA256,
		Duplicate: !inserted,
	})
}

type uploadMeta struct {
	filename string
	md5      string
	size     int64
	// capturedAt is when the photo was taken; modifiedAt is when the local asset
	// last changed. The second one is what tells a later sync/check that an
	// edited photo needs re-examining even though its local id never changed.
	capturedAt *time.Time
	modifiedAt *time.Time
	deviceID   string
	localID    string
}

func parseUploadHeaders(r *http.Request) (uploadMeta, error) {
	var m uploadMeta
	var err error

	if m.filename, err = requiredHeader(r, "X-Photo-Filename"); err != nil {
		return m, err
	}
	if m.md5, err = requiredHeader(r, "X-Photo-Md5"); err != nil {
		return m, err
	}
	if m.deviceID, err = requiredHeader(r, "X-Photo-Device-Id"); err != nil {
		return m, err
	}
	if m.localID, err = requiredHeader(r, "X-Photo-Local-Id"); err != nil {
		return m, err
	}

	rawSize, err := requiredHeader(r, "X-Photo-Size")
	if err != nil {
		return m, err
	}
	if m.size, err = strconv.ParseInt(rawSize, 10, 64); err != nil {
		return m, fmt.Errorf("X-Photo-Size is not a number: %q", rawSize)
	}
	if m.size < 0 {
		return m, fmt.Errorf("X-Photo-Size is negative: %d", m.size)
	}

	if m.capturedAt, err = optionalTimeHeader(r, "X-Photo-Captured-At"); err != nil {
		return m, err
	}
	if m.modifiedAt, err = optionalTimeHeader(r, "X-Photo-Modified-At"); err != nil {
		return m, err
	}
	return m, nil
}

func optionalTimeHeader(r *http.Request, name string) (*time.Time, error) {
	raw := strings.TrimSpace(r.Header.Get(name))
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not RFC3339: %q", name, raw)
	}
	t = t.UTC()
	return &t, nil
}

func requiredHeader(r *http.Request, name string) (string, error) {
	v := strings.TrimSpace(r.Header.Get(name))
	if v == "" {
		return "", fmt.Errorf("missing required header %s", name)
	}
	return v, nil
}
