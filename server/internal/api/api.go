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
	"sync"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
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
	// Devices authenticates the write path. Without it the write endpoints
	// answer 503 rather than running unauthenticated, so forgetting to wire it
	// takes the archive offline instead of opening it.
	Devices *devices.Store
	// Uploads holds partially-received originals. Optional: without it the
	// resumable endpoints answer 404 and single-shot uploads still work, which
	// is the right degradation for a server whose staging directory is gone.
	Uploads *uploads.Store
	// Nudge wakes the derivative workers after an upload commits, so the first
	// thumbnail appears in about the time it takes to make one rather than at
	// the next poll. Optional: without it the pools still find the work.
	Nudge func()
	Log   *slog.Logger

	pairOnce    sync.Once
	pairLimiter *attemptLimiter
}

// Handler serves the whole API, and must only be mounted on a listener whose
// traffic is encrypted. Everything that carries a device token is here.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/pair", s.handlePair)

	mux.HandleFunc("POST /v1/sync/check", s.requireDevice(s.handleSyncCheck))
	mux.HandleFunc("POST /v1/assets", s.requireDevice(s.handleUpload))

	// Resumable uploads. The id is derived from the declaration, so POST
	// /v1/uploads is both "begin" and "where did I get to".
	mux.HandleFunc("POST /v1/uploads", s.requireDevice(s.handleCreateUpload))
	mux.HandleFunc("GET /v1/uploads/{id}", s.requireDevice(s.handleUploadStatus))
	mux.HandleFunc("PUT /v1/uploads/{id}", s.requireDevice(s.handleUploadChunk))
	mux.HandleFunc("POST /v1/uploads/{id}/commit", s.requireDevice(s.handleCommitUpload))
	mux.HandleFunc("DELETE /v1/uploads/{id}", s.requireDevice(s.handleAbortUpload))

	s.readRoutes(mux)
	return mux
}

// PlaintextHandler serves only what is safe to send in the clear: the gallery's
// read endpoints and health.
//
// The write endpoints are present but refuse, and pairing is absent entirely, so
// a device token cannot travel unencrypted no matter where this listener is
// bound. That is a property of the routing table rather than of a check inside a
// handler, which is why widening PLAINTEXT_ADDR to the LAN — a thing the gallery
// may legitimately want — cannot accidentally expose a credential.
//
// What it does expose is the archive, to anyone who can reach it. That is the
// deliberate Phase 5 scope: the write path is closed, the read path is not yet.
func (s *Server) PlaintextHandler() http.Handler {
	mux := http.NewServeMux()

	for _, route := range []string{
		"POST /v1/pair",
		"POST /v1/sync/check",
		"POST /v1/assets",
		"POST /v1/uploads",
		"GET /v1/uploads/{id}",
		"PUT /v1/uploads/{id}",
		"POST /v1/uploads/{id}/commit",
		"DELETE /v1/uploads/{id}",
	} {
		mux.HandleFunc(route, refuseInsecure)
	}

	s.readRoutes(mux)
	return mux
}

// readRoutes are the endpoints the gallery is built on, plus health. None of
// them authenticates anything, on either listener.
func (s *Server) readRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/timeline", s.handleTimeline)
	mux.HandleFunc("POST /v1/timeline/states", s.handleTimelineStates)

	mux.HandleFunc("GET /v1/assets/{id}", s.handleAssetDetail)
	mux.HandleFunc("GET /v1/assets/{id}/original", s.handleOriginal)
	mux.HandleFunc("GET /v1/assets/{id}/thumb", s.handleThumb)
	mux.HandleFunc("GET /v1/assets/{id}/preview", s.handlePreview)
	mux.HandleFunc("GET /v1/assets/{id}/playback", s.handlePlayback)

	mux.HandleFunc("GET /v1/jobs", s.handleJobs)
	// Unauthenticated on purpose: the app pings this to decide whether a
	// remembered address still answers, which it has to be able to do before it
	// holds a token at all.
	mux.HandleFunc("GET /health", s.handleHealth)
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
