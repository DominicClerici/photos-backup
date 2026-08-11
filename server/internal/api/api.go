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
	"github.com/dominicclerici/photos-backup/server/internal/livecache"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/video"
)

type Server struct {
	Store       *db.Store
	Blobs       *blobstore.Store
	Derivatives *derivstore.Store
	Manifest    *manifest.Log
	Converter   *derive.Converter
	// Video renders a Live Photo's playable rendition on demand. Optional: the
	// live preview endpoint 404s without it, and the gallery falls back to the
	// still, which is the same degradation a host with no ffmpeg already has.
	Video *video.Tool
	// LivePreviews holds those renditions for as long as a viewer is likely to
	// ask for them again. Optional alongside Video, and required by nothing
	// else.
	LivePreviews *livecache.Cache
	Queue        *jobs.Queue
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

	// A read, but not one readRoutes can carry. Every other read answers the
	// same thing to every paired device, which is why guard drops the identity
	// and why a device id only ever reaches the database through a
	// deviceHandler. This one is scoped to who is asking, so it takes the
	// authenticated path instead of widening guard to smuggle an identity
	// through the read routes.
	//
	// The cost is that the plaintext listener cannot serve it, so the browser
	// gallery cannot read these numbers. It does not show them today, and an
	// unauthenticated archive-wide variant for it is a separate decision.
	mux.HandleFunc("GET /v1/stats", s.requireDevice(s.handleStats))

	// Reads are authenticated here and open on the plaintext listener. Which
	// listener a request arrived on is the whole of the difference, so it is
	// settled once, at the routing table, rather than sampled inside handlers.
	s.readRoutes(mux, s.requireToken)
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
// What it does expose is the archive, unauthenticated, to anyone who can reach
// it. Since Phase 6 that is the only place the archive is readable without a
// token, which is why PLAINTEXT_ADDR stays on loopback: the browser gallery
// reaches it through the Next.js rewrite from the same machine, and nothing else
// should.
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
		// Not a write, but it needs a device token to know whose stats to
		// report, and a token is exactly what this listener will not accept.
		"GET /v1/stats",
	} {
		mux.HandleFunc(route, refuseInsecure)
	}

	s.readRoutes(mux, openToAnyone)
	return mux
}

// guard decides who may call a read route. There are two: a device token, or
// nobody in particular.
type guard func(http.HandlerFunc) http.HandlerFunc

// openToAnyone is the plaintext listener's guard. Named rather than passed as a
// bare identity function so the call site reads as a decision.
func openToAnyone(next http.HandlerFunc) http.HandlerFunc { return next }

// readRoutes are the endpoints the gallery is built on, plus health.
//
// Every one of them is behind the caller's guard except /health, which is
// unauthenticated on both listeners on purpose: the app pings it to decide
// whether a remembered address still answers, and it has to be able to do that
// before it holds a token at all.
func (s *Server) readRoutes(mux *http.ServeMux, allow guard) {
	mux.HandleFunc("GET /v1/timeline", allow(s.handleTimeline))
	mux.HandleFunc("POST /v1/timeline/states", allow(s.handleTimelineStates))

	mux.HandleFunc("GET /v1/assets/{id}", allow(s.handleAssetDetail))
	mux.HandleFunc("GET /v1/assets/{id}/original", allow(s.handleOriginal))
	mux.HandleFunc("GET /v1/assets/{id}/thumb", allow(s.handleThumb))
	mux.HandleFunc("GET /v1/assets/{id}/preview", allow(s.handlePreview))
	mux.HandleFunc("GET /v1/assets/{id}/playback", allow(s.handlePlayback))

	// A Live Photo's motion, addressed by the still's id. Two sizes for the two
	// places it plays: the grid's hover and the viewer's press-and-hold.
	mux.HandleFunc("GET /v1/assets/{id}/live/thumb", allow(s.handleLiveThumb))
	mux.HandleFunc("GET /v1/assets/{id}/live/preview", allow(s.handleLivePreview))

	// Guarded with the rest: a failed job carries a filename and an error string,
	// which is archive content by another name.
	mux.HandleFunc("GET /v1/jobs", allow(s.handleJobs))
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
