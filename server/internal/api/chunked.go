package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
)

// maxDeclarationBody is generous for a filename and a few timestamps.
const maxDeclarationBody = 64 << 10

type createSessionRequest struct {
	DeviceID          string     `json:"deviceId"`
	LocalID           string     `json:"localId"`
	Filename          string     `json:"filename"`
	MD5               string     `json:"md5"`
	Size              int64      `json:"size"`
	CapturedAt        *time.Time `json:"capturedAt"`
	ModifiedAt        *time.Time `json:"modifiedAt"`
	LiveParentLocalID string     `json:"liveParentLocalId"`
	ContentID         string     `json:"contentId"`
}

type sessionResponse struct {
	UploadID string `json:"uploadId"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	// Complete tells a client that every byte is already in, so it can go
	// straight to commit rather than probing with a zero-length chunk.
	Complete bool `json:"complete"`
}

// handleCreateUpload opens a resumable session, or hands back the one that
// already exists for these bytes.
//
// It is the same call for "start" and "resume" because the id is derived from
// the declaration rather than allocated. A phone that was killed mid-video, and
// therefore knows nothing about the transfer beyond the file it was sending,
// asks this and is told how far it got.
func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request, device devices.Device) {
	if s.Uploads == nil {
		writeError(w, http.StatusNotFound, "resumable uploads are not enabled on this server")
		return
	}

	var req createSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeclarationBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	// The authenticated id, not the declared one — it is part of what the
	// session id is derived from, so this is also what scopes a session to the
	// device that opened it.
	deviceID, ok := s.deviceIDFor(w, req.DeviceID, device)
	if !ok {
		return
	}

	if req.LiveParentLocalID != "" && req.LiveParentLocalID == req.LocalID {
		writeError(w, http.StatusBadRequest, "liveParentLocalId names this same asset")
		return
	}

	session, err := s.Uploads.Create(uploads.Declaration{
		DeviceID:          deviceID,
		LocalID:           req.LocalID,
		Filename:          req.Filename,
		MD5:               req.MD5,
		Size:              req.Size,
		CapturedAt:        normalizeTime(req.CapturedAt),
		ModifiedAt:        normalizeTime(req.ModifiedAt),
		LiveParentLocalID: req.LiveParentLocalID,
		ContentID:         req.ContentID,
	})
	if err != nil {
		if isDeclarationError(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.logger().Error("create upload session", "error", err)
		writeError(w, http.StatusInternalServerError, "could not open an upload session")
		return
	}

	writeJSON(w, http.StatusOK, sessionOf(session))
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request, device devices.Device) {
	session, ok := s.session(w, r, device)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sessionOf(session))
}

// handleUploadChunk appends one chunk and answers with the new offset.
//
// A chunk that starts anywhere other than the current offset is refused with
// 409 and the real offset, so a client whose idea of progress has drifted
// re-seeks instead of restarting a 550MB video.
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request, device devices.Device) {
	// Ownership is settled before a byte of the body is read, so a chunk aimed
	// at another device's session is refused rather than absorbed.
	existing, ok := s.session(w, r, device)
	if !ok {
		return
	}
	id := existing.ID

	offset, err := chunkOffset(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	session, err := s.Uploads.Append(id, offset, r.Body)
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such upload session")
		return
	case errors.Is(err, uploads.ErrOffsetMismatch):
		// The body carries the offset, so recovery needs no second round trip.
		writeJSON(w, http.StatusConflict, sessionOf(session))
		return
	case errors.Is(err, uploads.ErrTooLong):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		// The bytes that landed are still good and still fsynced. Report the
		// offset rather than an opaque failure: the client resumes from there.
		s.logger().Warn("chunk interrupted", "error", err, "upload_id", id, "offset", session.Offset)
		writeJSON(w, http.StatusServiceUnavailable, sessionOf(session))
		return
	}

	writeJSON(w, http.StatusOK, sessionOf(session))
}

// handleCommitUpload turns a complete session into an archived asset. Past this
// point the two upload paths are the same code: the blob is verified and
// renamed into the tree, then finishUpload writes the manifest line and the row.
func (s *Server) handleCommitUpload(w http.ResponseWriter, r *http.Request, device devices.Device) {
	session, ok := s.session(w, r, device)
	if !ok {
		return
	}
	if !session.Complete() {
		writeJSON(w, http.StatusConflict, sessionOf(session))
		return
	}

	path := s.Uploads.PartPath(session.ID)
	contentType, ext := mediatype.Detect(session.Decl.Filename, headOf(path))

	res, err := s.Blobs.Adopt(path, ext, blobstore.Expected{
		MD5:  session.Decl.MD5,
		Size: session.Decl.Size,
	})
	switch {
	case errors.Is(err, blobstore.ErrChecksumMismatch), errors.Is(err, blobstore.ErrSizeMismatch):
		// The assembled file is not what was promised, so it is worth nothing
		// and holding it would only block the retry. Discarding drops the
		// session back to offset zero.
		if discardErr := s.Uploads.Discard(session.ID); discardErr != nil {
			s.logger().Error("discard failed session", "error", discardErr, "upload_id", session.ID)
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.logger().Error("adopt staged upload", "error", err, "upload_id", session.ID)
		writeError(w, http.StatusInternalServerError, "could not store upload")
		return
	}

	// Adopt consumed the part file; the sidecar is all that is left.
	if err := s.Uploads.Discard(session.ID); err != nil {
		s.logger().Warn("could not clear committed session", "error", err, "upload_id", session.ID)
	}

	s.finishUpload(w, r.Context(), res, ext, contentType, uploadMeta{
		filename:          session.Decl.Filename,
		md5:               res.MD5,
		size:              res.Size,
		capturedAt:        session.Decl.CapturedAt,
		modifiedAt:        session.Decl.ModifiedAt,
		deviceID:          session.Decl.DeviceID,
		localID:           session.Decl.LocalID,
		liveParentLocalID: session.Decl.LiveParentLocalID,
		contentID:         session.Decl.ContentID,
	})
}

func (s *Server) handleAbortUpload(w http.ResponseWriter, r *http.Request, device devices.Device) {
	session, ok := s.session(w, r, device)
	if !ok {
		return
	}
	if err := s.Uploads.Discard(session.ID); err != nil && !errors.Is(err, uploads.ErrNotFound) {
		s.logger().Error("discard upload session", "error", err)
		writeError(w, http.StatusInternalServerError, "could not discard the session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// session loads the session named in the path, and only if the calling device is
// the one that opened it.
func (s *Server) session(w http.ResponseWriter, r *http.Request, device devices.Device) (uploads.Session, bool) {
	if s.Uploads == nil {
		writeError(w, http.StatusNotFound, "resumable uploads are not enabled on this server")
		return uploads.Session{}, false
	}
	session, err := s.Uploads.Get(r.PathValue("id"))
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such upload session")
		return uploads.Session{}, false
	case err != nil:
		s.logger().Error("load upload session", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the upload session")
		return uploads.Session{}, false
	}
	if !s.ownedBy(w, session.Decl.DeviceID, device) {
		return uploads.Session{}, false
	}
	return session, true
}

func sessionOf(s uploads.Session) sessionResponse {
	return sessionResponse{
		UploadID: s.ID,
		Offset:   s.Offset,
		Size:     s.Decl.Size,
		Complete: s.Complete(),
	}
}

// headOf reads what mediatype.Sniff wants off a staged file. A file that cannot
// be read here will fail loudly a moment later in Adopt, so this stays quiet.
func headOf(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	buf := make([]byte, mediatype.SniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil
	}
	return buf[:n]
}

var contentRange = regexp.MustCompile(`^bytes (\d+)-(\d+)/(\d+|\*)$`)

// chunkOffset reads where a chunk claims to start.
//
// Content-Range is the canonical form; X-Upload-Offset is accepted because a
// client that can only set simple headers should not be locked out of resuming.
func chunkOffset(r *http.Request) (int64, error) {
	if raw := r.Header.Get("X-Upload-Offset"); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			return 0, fmt.Errorf("X-Upload-Offset is not a byte offset: %q", raw)
		}
		return offset, nil
	}

	raw := r.Header.Get("Content-Range")
	if raw == "" {
		return 0, errors.New("a chunk needs a Content-Range or X-Upload-Offset header")
	}
	m := contentRange.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("Content-Range is not `bytes start-end/total`: %q", raw)
	}
	offset, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Content-Range start is not a number: %q", raw)
	}
	return offset, nil
}

// isDeclarationError separates "the client asked for something impossible" from
// "the disk is unhappy", which is the difference between a 400 and a 500.
func isDeclarationError(err error) bool {
	var pathErr *os.PathError
	return !errors.As(err, &pathErr)
}
