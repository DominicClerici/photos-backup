package api

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
)

// Every byte this file serves is a pure function of a SHA-256, so it can be
// cached forever. Editing a photo on the phone produces different bytes, a
// different digest, and a different asset id — a URL's content can never
// change under a client.
const immutable = "public, max-age=31536000, immutable"

func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	f, err := s.Blobs.Open(asset.SHA256, asset.Ext)
	if err != nil {
		s.logger().Error("open blob", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "blob missing from disk")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Cache-Control", immutable)
	w.Header().Set("ETag", etagFor(asset.SHA256, "original"))
	// ServeContent gives Range support for free, which is what lets a browser
	// scrub a video served straight from the archive.
	http.ServeContent(w, r, asset.OriginalFilename, asset.UploadedAt, f)
}

// handleThumb serves the stored 256px rendition.
//
// It answers from the file rather than from derived_state, so a photo whose
// metadata job failed after the thumbnail was written still shows its picture.
// A missing file is a 404 the client turns into a placeholder tile.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derivstore.Thumb, "image/webp")
}

// handlePlayback serves the browser-playable rendition of a video.
func (s *Server) handlePlayback(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if asset.MediaKind != db.MediaVideo {
		writeError(w, http.StatusNotFound, "not a video")
		return
	}
	s.serveDerivative(w, r, asset, derivstore.Playback, "video/mp4")
}

func (s *Server) serveDerivative(w http.ResponseWriter, r *http.Request, asset db.Asset, suffix, contentType string) {
	f, err := s.Derivatives.Open(asset.SHA256, suffix)
	if err != nil {
		if os.IsNotExist(err) {
			// Not an error condition: the job may simply not have run yet.
			writeError(w, http.StatusNotFound, "derivative not generated yet")
			return
		}
		s.logger().Error("open derivative", "error", err, "sha256", asset.SHA256, "suffix", suffix)
		writeError(w, http.StatusInternalServerError, "could not read derivative")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.logger().Error("stat derivative", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "could not read derivative")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", immutable)
	w.Header().Set("ETag", etagFor(asset.SHA256, suffix))
	http.ServeContent(w, r, asset.SHA256+suffix, info.ModTime(), f)
}

// handlePreview renders the 2048px rendition on demand and stores nothing.
//
// The conditional check happens before the conversion, not after: a client that
// already has these bytes must not cost an ImageMagick process to be told so.
// That is what makes arrow-keying back through a viewer cheap.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if asset.MediaKind == db.MediaVideo {
		writeError(w, http.StatusNotFound, "videos have no preview; use /playback")
		return
	}

	etag := etagFor(asset.SHA256, "preview")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", immutable)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Buffered rather than streamed: a conversion that fails halfway must
	// produce an honest status code, not a truncated image with a 200 on it.
	var buf bytes.Buffer
	if err := s.Converter.Preview(r.Context(), s.Blobs.Path(asset.SHA256, asset.Ext), &buf); err != nil {
		if r.Context().Err() != nil {
			return // client went away mid-conversion
		}
		s.logger().Error("render preview", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "could not render preview")
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	http.ServeContent(w, r, asset.SHA256+".preview.webp", asset.UploadedAt, bytes.NewReader(buf.Bytes()))
}

func etagFor(sha256hex, variant string) string {
	return fmt.Sprintf(`"%s-%s"`, sha256hex, strings.TrimPrefix(variant, "."))
}

// etagMatches implements the If-None-Match comparison we need: a list of tags,
// or "*". Weak-comparison prefixes are ignored because nothing here issues them.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}
