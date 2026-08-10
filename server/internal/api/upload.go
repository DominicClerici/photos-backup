package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
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

	// Classification has to happen before the blob is stored, because the
	// content type decides the extension and the extension decides the path.
	// Peeking off a buffered reader keeps this to one look at the first 512
	// bytes, with the body still fully streamable afterwards.
	body := bufio.NewReaderSize(r.Body, mediatype.SniffLen)
	head, err := body.Peek(mediatype.SniffLen)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		writeError(w, http.StatusBadRequest, "could not read upload body: "+err.Error())
		return
	}
	contentType, ext := mediatype.Detect(meta.filename, head)

	res, err := s.Blobs.Put(body, ext, blobstore.Expected{MD5: meta.md5, Size: meta.size})
	switch {
	case errors.Is(err, blobstore.ErrChecksumMismatch), errors.Is(err, blobstore.ErrSizeMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.logger().Error("store blob", "error", err)
		writeError(w, http.StatusInternalServerError, "could not store upload")
		return
	}

	s.finishUpload(w, r.Context(), res, ext, contentType, meta)
}

// finishUpload is everything after the bytes are safely on disk: the manifest
// line, the database row, and the nudge that wakes the derivative workers.
//
// Both upload paths end here. A single-shot POST and a committed chunked
// session differ only in how the blob arrived, and giving them separate commit
// code is how you end up with two commit orderings and tests for one of them.
func (s *Server) finishUpload(
	w http.ResponseWriter,
	ctx context.Context,
	res blobstore.Result,
	ext, contentType string,
	meta uploadMeta,
) {
	if res.Created {
		entry := manifest.Entry{
			SHA256:      res.SHA256,
			MD5:         res.MD5,
			Size:        res.Size,
			Filename:    meta.filename,
			ContentType: contentType,
			Ext:         ext,
			CapturedAt:  meta.capturedAt,
			ModifiedAt:  meta.modifiedAt,
			DeviceID:    meta.deviceID,
			LocalID:     meta.localID,
			StoredAt:    time.Now().UTC(),
		}
		if err := s.Manifest.Append(entry); err != nil {
			s.logger().Error("append manifest", "error", err, "sha256", res.SHA256)
			writeError(w, http.StatusInternalServerError, "could not record upload")
			return
		}
	}

	id, inserted, err := s.Store.RecordAsset(ctx, db.Asset{
		SHA256:           res.SHA256,
		MD5:              res.MD5,
		ByteSize:         res.Size,
		OriginalFilename: meta.filename,
		Ext:              ext,
		ContentType:      contentType,
		MediaKind:        mediatype.Kind(contentType),
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
	return normalizeTime(&t), nil
}

// normalizeTime rounds a client-supplied timestamp to a precision the database
// and every later comparison can agree on.
//
// This matters more than it looks. A device mapping is only trusted while its
// stored modified_at still equals what the phone reports, and that equality is
// exact. Go carries nanoseconds, Postgres stores microseconds, and JSON and
// RFC3339 headers disagree about how many decimals to write — so a client that
// formats the same instant two different ways stores one and asks about the
// other, is told "unknown" forever, and re-hashes its entire library on every
// single run. Truncating on the way in makes the comparison stable regardless
// of how the client spelled it.
//
// Milliseconds, because that is the precision an iOS asset date actually has
// once it has been through JavaScript, and pretending to more is inviting the
// same mismatch back in a smaller form.
func normalizeTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	rounded := t.UTC().Truncate(time.Millisecond)
	return &rounded
}

func requiredHeader(r *http.Request, name string) (string, error) {
	v := strings.TrimSpace(r.Header.Get(name))
	if v == "" {
		return "", fmt.Errorf("missing required header %s", name)
	}
	return v, nil
}
