package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/mediatype"
)

// The browser gallery's own upload, which is a different act from a phone's
// backup and is why it is a different endpoint rather than a token handed to a
// web page.
//
// A backup is continuous, unattended, and about a library that already exists
// somewhere else: it carries a device identity, it declares a local id so the
// next run knows what it has already sent, and it resumes a 3GB video across a
// dropped connection. None of that describes somebody dragging eleven files
// onto a page. What that person has is bytes and a filename, and what they need
// back is whether each one landed.
//
// So this route takes the same commit path — internal/api.finishUpload, byte for
// byte — and declares nothing it cannot honestly declare. See galleryDeviceID
// for what it records instead of a device.
const (
	// galleryDeviceID stands in assets.device_id for anything the browser sent.
	//
	// Not a real devices.id and deliberately not shaped like one: every paired
	// device is a uuid, so a literal word cannot collide with one now or after
	// any number of pairings. It is provenance rather than identity — there is
	// no token behind it and nothing authenticates as it.
	galleryDeviceID = "web"

	// maxCheckDigests bounds one duplicate check. The page hashes files as they
	// are dropped and asks in one batch; a thousand photographs at a time is
	// more than anybody drags and small enough to answer from an index.
	maxCheckDigests = 1000
	// maxCheckDigestBody is generous for that many 64-character digests.
	maxCheckDigestBody = 128 << 10
)

type contentCheckRequest struct {
	SHA256 []string `json:"sha256"`
}

// contentCheckMatch is one digest the archive already holds. Filename is absent
// for a vault or purged match — see db.LookupContent for why.
type contentCheckMatch struct {
	SHA256   string `json:"sha256"`
	Where    string `json:"where"`
	AssetID  string `json:"id,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type contentCheckResponse struct {
	Known []contentCheckMatch `json:"known"`
}

// handleGalleryCheck answers which of these digests the archive already has.
//
// This is the browser's version of sync/check, and it asks a narrower question
// with a better key. The phone asks by (md5, size) because hashing its whole
// library is the cost it is trying to avoid; the browser is holding the exact
// bytes it is about to send and has already read every one of them, so it can
// offer the archive's own sha256 and get an answer that cannot be a collision.
//
// It exists so a duplicate is a row on the page before anything is transferred,
// rather than a result after. Sending the file anyway and reading `duplicate`
// off the response would be correct and would also mean uploading four hundred
// megabytes to be told the archive already had them.
func (s *Server) handleGalleryCheck(w http.ResponseWriter, r *http.Request) {
	var req contentCheckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCheckDigestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}
	if len(req.SHA256) > maxCheckDigests {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%d digests exceeds the %d-digest limit", len(req.SHA256), maxCheckDigests))
		return
	}

	digests := make([]string, 0, len(req.SHA256))
	for i, raw := range req.SHA256 {
		sha, err := normalizeDigest(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("sha256[%d]: %s", i, err))
			return
		}
		digests = append(digests, sha)
	}

	found, err := s.Store.LookupContent(r.Context(), digests)
	if err != nil {
		s.logger().Error("look up content", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	// Ordered as asked rather than as the database returned them, so the page
	// can walk its own list beside this one.
	known := make([]contentCheckMatch, 0, len(found))
	for _, sha := range digests {
		if m, ok := found[sha]; ok {
			known = append(known, contentCheckMatch{
				SHA256:   m.SHA256,
				Where:    m.Where,
				AssetID:  m.AssetID,
				Filename: m.Filename,
			})
		}
	}
	writeJSON(w, http.StatusOK, contentCheckResponse{Known: known})
}

// handleGalleryUpload stores one original sent from the browser.
//
// Single-shot, with no resumable sibling. A chunked session exists so a phone
// on a train can finish a video it started yesterday; a browser tab that is
// closed mid-upload has taken the file picker's selection with it, and there is
// nothing left to resume from. Retrying is one click on a page that is still
// listing the file.
func (s *Server) handleGalleryUpload(w http.ResponseWriter, r *http.Request) {
	meta, err := parseGalleryHeaders(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	body := bufio.NewReaderSize(r.Body, mediatype.SniffLen)
	head, err := body.Peek(mediatype.SniffLen)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		writeError(w, http.StatusBadRequest, "could not read upload body: "+err.Error())
		return
	}
	contentType, ext := mediatype.Detect(meta.filename, head)

	// The one rule this path has that the device path does not.
	//
	// mediatype's own comment is right that an archive refusing what it cannot
	// classify is not an archive, and the phone endpoint keeps that rule: what
	// arrives there is a photo library, and a blob nobody can name is still a
	// photograph somebody took. What arrives here is whatever was dropped on a
	// web page, and a spreadsheet that lands in the blob tree is there forever —
	// stored, backed up, and shown as a grey tile that will never render.
	//
	// Octet is exactly the "neither the name nor the bytes said anything" case,
	// so this refuses that and nothing else. A Takeout Live Photo video with no
	// extension still sniffs as video/mp4 and still goes in.
	if contentType == mediatype.Octet {
		writeError(w, http.StatusUnsupportedMediaType,
			fmt.Sprintf("%q is not a photo or a video this archive recognises", meta.filename))
		return
	}

	res, err := s.Blobs.Put(body, ext, blobstore.Expected{SHA256: meta.sha256, Size: meta.size})
	switch {
	case errors.Is(err, blobstore.ErrChecksumMismatch), errors.Is(err, blobstore.ErrSizeMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.logger().Error("store gallery blob", "error", err)
		writeError(w, http.StatusInternalServerError, "could not store upload")
		return
	}

	s.finishUpload(w, r.Context(), res, ext, contentType, uploadMeta{
		filename:   meta.filename,
		size:       meta.size,
		capturedAt: meta.capturedAt,
		modifiedAt: meta.modifiedAt,
		deviceID:   galleryDeviceID,
		// No local id, and so no device_assets row. There is nothing on the
		// other end to sync with: the file came out of a picker and the page
		// that sent it will not exist tomorrow. Writing a mapping against a
		// made-up local id would put a permanent row in the table sync/check
		// reads, keyed to something that will never ask again.
		localID: "",
	})
}

// galleryMeta is what a browser can honestly say about a file it is sending.
//
// Shorter than uploadMeta by everything only a phone knows: no local id, no
// device, no Live Photo parent, no content identifier. A File in a browser is a
// name, a length, and a last-modified date, and this is those three.
type galleryMeta struct {
	filename string
	// sha256 is optional, and is the integrity check when it is there. The
	// browser computes it anyway to ask handleGalleryCheck about duplicates, so
	// sending it costs a header; a client that skips the check is still held to
	// the declared length.
	sha256     string
	size       int64
	capturedAt *time.Time
	modifiedAt *time.Time
}

func parseGalleryHeaders(r *http.Request) (galleryMeta, error) {
	var m galleryMeta
	var err error

	if m.filename, err = requiredHeader(r, "X-Photo-Filename"); err != nil {
		return m, err
	}
	if raw := strings.TrimSpace(r.Header.Get("X-Photo-Sha256")); raw != "" {
		if m.sha256, err = normalizeDigest(raw); err != nil {
			return m, fmt.Errorf("X-Photo-Sha256: %w", err)
		}
	}

	rawSize, err := requiredHeader(r, "X-Photo-Size")
	if err != nil {
		return m, err
	}
	if m.size, err = strconv.ParseInt(rawSize, 10, 64); err != nil {
		return m, fmt.Errorf("X-Photo-Size is not a number: %q", rawSize)
	}
	if m.size <= 0 {
		return m, fmt.Errorf("X-Photo-Size must be positive: %d", m.size)
	}

	// A browser has no capture time — that is in the file, and the metadata job
	// will read it. What it has is the filesystem's last-modified date, which is
	// sent as both: it is a far better guess at where a screenshot belongs on
	// the timeline than the moment it was uploaded, and sort_time prefers
	// whatever EXIF says over either of them anyway.
	if m.capturedAt, err = optionalTimeHeader(r, "X-Photo-Captured-At"); err != nil {
		return m, err
	}
	if m.modifiedAt, err = optionalTimeHeader(r, "X-Photo-Modified-At"); err != nil {
		return m, err
	}
	return m, nil
}

// normalizeDigest lowercases a hex sha256 and rejects anything that is not one.
// Validated rather than passed through because it reaches a query and a
// comparison, and "is this 64 hex characters" is the whole of what makes it
// safe to do either with.
func normalizeDigest(raw string) (string, error) {
	sha := strings.ToLower(strings.TrimSpace(raw))
	if len(sha) != 64 {
		return "", fmt.Errorf("not a sha256: %d characters, want 64", len(sha))
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("not a sha256: %q is not hexadecimal", raw)
		}
	}
	return sha, nil
}
