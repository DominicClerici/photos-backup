// Package api serves the upload endpoints, the derivative endpoints, and the
// JSON the gallery is built on. It does HTTP and nothing else: no image
// processing, no queue mechanics, no SQL beyond calling the store.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
)

type Server struct {
	Store       *db.Store
	Blobs       *blobstore.Store
	Derivatives *derivstore.Store
	Manifest    *manifest.Log
	Converter   *derive.Converter
	Queue       *jobs.Queue
	// Uploads holds partially-received originals. Optional: without it the
	// resumable endpoints answer 404 and single-shot uploads still work, which
	// is the right degradation for a server whose staging directory is gone.
	Uploads *uploads.Store
	// Nudge wakes the derivative workers after an upload commits, so the first
	// thumbnail appears in about the time it takes to make one rather than at
	// the next poll. Optional: without it the pools still find the work.
	Nudge func()
	Log   *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/sync/check", s.handleSyncCheck)
	mux.HandleFunc("POST /v1/assets", s.handleUpload)

	// Resumable uploads. The id is derived from the declaration, so POST
	// /v1/uploads is both "begin" and "where did I get to".
	mux.HandleFunc("POST /v1/uploads", s.handleCreateUpload)
	mux.HandleFunc("GET /v1/uploads/{id}", s.handleUploadStatus)
	mux.HandleFunc("PUT /v1/uploads/{id}", s.handleUploadChunk)
	mux.HandleFunc("POST /v1/uploads/{id}/commit", s.handleCommitUpload)
	mux.HandleFunc("DELETE /v1/uploads/{id}", s.handleAbortUpload)

	mux.HandleFunc("GET /v1/timeline", s.handleTimeline)
	mux.HandleFunc("POST /v1/timeline/states", s.handleTimelineStates)

	mux.HandleFunc("GET /v1/assets/{id}", s.handleAssetDetail)
	mux.HandleFunc("GET /v1/assets/{id}/original", s.handleOriginal)
	mux.HandleFunc("GET /v1/assets/{id}/thumb", s.handleThumb)
	mux.HandleFunc("GET /v1/assets/{id}/preview", s.handlePreview)
	mux.HandleFunc("GET /v1/assets/{id}/playback", s.handlePlayback)

	mux.HandleFunc("GET /v1/jobs", s.handleJobs)
	mux.HandleFunc("GET /health", s.handleHealth)

	return mux
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) nudge() {
	if s.Nudge != nil {
		s.Nudge()
	}
}

// lookup resolves the {id} path value, answering with the right status when it
// is unknown or the database is down.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (db.Asset, bool) {
	asset, err := s.Store.Asset(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such asset")
		return db.Asset{}, false
	case err != nil:
		// An unparseable id reaches the database as a bad uuid cast rather than
		// as ErrNotFound, and answering 503 for it would be a lie.
		if isBadUUID(err) {
			writeError(w, http.StatusNotFound, "no such asset")
			return db.Asset{}, false
		}
		s.logger().Error("load asset", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return db.Asset{}, false
	}
	return asset, true
}

func isBadUUID(err error) bool {
	return strings.Contains(err.Error(), "invalid input syntax for type uuid") ||
		strings.Contains(err.Error(), "invalid UUID")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
