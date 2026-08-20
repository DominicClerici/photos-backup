// Package api serves the upload endpoints, the derivative endpoints, and the
// JSON the gallery is built on. It does HTTP and nothing else: no image
// processing, no queue mechanics, no SQL beyond calling the store.
package api

import (
	"context"
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
	"github.com/dominicclerici/photos-backup/server/internal/merge"
	"github.com/dominicclerici/photos-backup/server/internal/uploads"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
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
	// Vault is the Archive and Hidden buckets. Optional: without it the vault
	// endpoints answer 404 and the gallery draws the two rows as unavailable,
	// which is the right degradation for a server whose encrypted trees are on
	// a disk that is not mounted.
	Vault *vault.Service
	// Nudge wakes the derivative workers after an upload commits, so the first
	// thumbnail appears in about the time it takes to make one rather than at
	// the next poll. Optional: without it the pools still find the work.
	Nudge func()
	// Scan re-runs the search for things that ought to be one item and are
	// several — see internal/merge. A function rather than a worker, because
	// this package does HTTP and the worker package does not know it exists;
	// the result type is the pure one both of them can name.
	//
	// Optional. Without it the review endpoints still read and still resolve
	// groups, and only the button that looks for more answers 503, which is the
	// right degradation for an API server running with WORKER_DISABLED.
	Scan func(ctx context.Context) (merge.ScanResult, error)
	Log  *slog.Logger

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
	// What an export knew that the file does not. A write, and one that names
	// an asset rather than creating one, which is why it hangs off the asset
	// rather than sitting beside the upload endpoints.
	mux.HandleFunc("POST /v1/assets/{id}/import-metadata", s.requireDevice(s.handleImportMetadata))
	// What an export knew and the import could not attach to anything. It names
	// no asset — a sidecar orphan has none by definition — so unlike the route
	// above it sits on its own rather than under /v1/assets.
	mux.HandleFunc("POST /v1/import/orphans", s.requireDevice(s.handleImportOrphan))

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
	s.galleryRoutes(mux, s.requireToken)
	return mux
}

// PlaintextHandler serves only what is safe to send in the clear: the gallery's
// own endpoints and health.
//
// The device endpoints are present but refuse, and pairing is absent entirely,
// so a device token cannot travel unencrypted no matter where this listener is
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
		"POST /v1/assets/{id}/import-metadata",
		"POST /v1/import/orphans",
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
	s.galleryRoutes(mux, openToAnyone)
	return mux
}

// galleryRoutes are the writes the browser makes: delete, restore, purge, the
// vault, and making and filling albums.
//
// They are here rather than beside the upload endpoints because of what they
// are not. Every route above is about a device — it carries a token, it names a
// local id, it says which phone is asking — and refusing all of them on the
// plaintext listener is what keeps a credential off the wire. These carry no
// credential and name no device. They are the gallery acting on the archive,
// and the gallery is a browser on this machine talking to a loopback port.
//
// Which means the guard here is the same one the reads get, and the exposure is
// the same exposure: anyone who can reach PLAINTEXT_ADDR can already read every
// photograph in the archive, and can now also move them to the trash. That is a
// widening, and it is why PLAINTEXT_ADDR stays on loopback — the trash is
// undoable for a year, but "readable by anyone on the LAN" and "deletable by
// anyone on the LAN" are not the same sentence. Authenticating the gallery was
// the fix, and it is done — on the TLS listener, where a browser signs in with
// the gallery password and gets a cookie these routes accept. This listener did
// not change: it is loopback, and it is open.
func (s *Server) galleryRoutes(mux *http.ServeMux, allow guard) {
	mux.HandleFunc("POST /v1/trash", allow(s.handleTrash))
	mux.HandleFunc("POST /v1/trash/restore", allow(s.handleRestore))
	// Not DELETE /v1/trash: a purge names a selection, and a selection is a
	// body. The method that says "destroy this" is the one that cannot carry
	// what to destroy.
	mux.HandleFunc("POST /v1/trash/purge", allow(s.handlePurge))
	mux.HandleFunc("DELETE /v1/collections/albums/{id}", allow(s.handleDeleteAlbum))

	// Resolving a duplicate group, and undoing either kind of merge. They are
	// here rather than beside the reads above for the reason everything in this
	// table is: they move photographs to the trash, which is the same authority
	// the delete endpoints need and the same exposure.
	//
	// The scan is a write too, and the odd one out: it destroys nothing and
	// creates nothing anybody can see, but it queues the joins that put a
	// Snapchat recording back together without anyone approving it. That is a
	// change to the library, so it goes where the changes go.
	mux.HandleFunc("POST /v1/merges/scan", allow(s.handleScan))
	mux.HandleFunc("POST /v1/merges/{id}/merge", allow(s.handleMerge))
	mux.HandleFunc("POST /v1/merges/{id}/dismiss", allow(s.handleDismissMerge))
	mux.HandleFunc("POST /v1/merges/{id}/undo", allow(s.handleUnmerge))

	// Albums, which until now only an import could make. The membership routes
	// are POST and DELETE on a sub-resource rather than two verbs on the album
	// itself, because what they change is what is *in* it — deleting the album
	// is the route above and means something else entirely.
	mux.HandleFunc("POST /v1/collections/albums", allow(s.handleCreateAlbum))
	mux.HandleFunc("POST /v1/collections/albums/{id}/items", allow(s.handleAlbumAdd))
	mux.HandleFunc("DELETE /v1/collections/albums/{id}/items", allow(s.handleAlbumRemove))

	// The vault. Its writes are here for the same reason the trash's are — they
	// carry no credential and name no device — but they split along a line the
	// rest of this table does not have: everything that puts a photograph in
	// works on a locked vault, and everything that reads one answers 423 until
	// somebody types the password. See internal/api/vault.go.
	mux.HandleFunc("GET /v1/vault", allow(s.handleVaultStatus))
	mux.HandleFunc("POST /v1/vault/setup", allow(s.handleVaultSetup))
	mux.HandleFunc("POST /v1/vault/unlock", allow(s.handleVaultUnlock))
	mux.HandleFunc("POST /v1/vault/lock", allow(s.handleVaultLock))
	mux.HandleFunc("POST /v1/vault/password", allow(s.handleVaultPassword))
	mux.HandleFunc("POST /v1/vault/restore", allow(s.handleVaultRestore))

	// {bucket} is "archive" or "hidden", validated against a closed list before
	// it reaches anything — the same rule a category key is held to, and for the
	// same reason: it ends up in a predicate and on a path.
	mux.HandleFunc("POST /v1/vault/{bucket}", allow(s.handleVaultAdd))
	mux.HandleFunc("POST /v1/vault/{bucket}/albums/{id}", allow(s.handleVaultAlbum))
	// The bucket's own albums. Same three writes as the library's, and a
	// different implementation underneath every one of them — see
	// internal/api/vaultalbums.go for why that is the honest shape.
	mux.HandleFunc("POST /v1/vault/{bucket}/albums", allow(s.handleVaultCreateAlbum))
	mux.HandleFunc("POST /v1/vault/{bucket}/albums/{id}/items", allow(s.handleVaultAlbumAdd))
	mux.HandleFunc("DELETE /v1/vault/{bucket}/albums/{id}/items", allow(s.handleVaultAlbumRemove))
	mux.HandleFunc("POST /v1/vault/{bucket}/people", allow(s.handleVaultPerson))
	mux.HandleFunc("GET /v1/vault/{bucket}/collections", allow(s.handleVaultCollections))
	mux.HandleFunc("GET /v1/vault/{bucket}/timeline", allow(s.handleVaultTimeline))
	mux.HandleFunc("GET /v1/vault/{bucket}/timeline/days", allow(s.handleVaultDays))
	mux.HandleFunc("GET /v1/vault/{bucket}/timeline/locate", allow(s.handleVaultLocate))
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
	// The shape of the whole timeline before any of it: the headings it will
	// draw and how many tiles hang under each. The gallery lays the entire grid
	// out from this, so it never has to guess a height it will have to correct.
	mux.HandleFunc("GET /v1/timeline/days", allow(s.handleTimelineDays))
	// The other direction: an id, from a link somebody shared, turned into the
	// position everything else here is addressed by.
	mux.HandleFunc("GET /v1/timeline/locate", allow(s.handleTimelineLocate))
	mux.HandleFunc("POST /v1/timeline/states", allow(s.handleTimelineStates))

	// The collections page and the heading of one album. The membership itself
	// is not here: an album is read back through /v1/timeline?album=, so it
	// pages, virtualizes and zooms exactly like the library does.
	mux.HandleFunc("GET /v1/collections", allow(s.handleCollections))
	mux.HandleFunc("GET /v1/collections/albums/{id}", allow(s.handleAlbum))
	// The albums alone, for the "Add to album" menu — which opens over a grid
	// and has no use for the people or the category covers.
	mux.HandleFunc("GET /v1/collections/albums", allow(s.handleAlbumList))

	mux.HandleFunc("GET /v1/assets/{id}", allow(s.handleAssetDetail))
	// Which albums hold this one photograph, by id. What puts the ticks in the
	// "Add to album" menu when exactly one thing is selected.
	mux.HandleFunc("GET /v1/assets/{id}/albums", allow(s.handleAssetAlbums))
	mux.HandleFunc("GET /v1/assets/{id}/original", allow(s.handleOriginal))
	mux.HandleFunc("GET /v1/assets/{id}/thumb", allow(s.handleThumb))
	// The other stored sizes, which the gallery picks between as it zooms. The
	// unsized route above is the base one and is not going anywhere: a client
	// that has no opinion about size — the phone app, a link someone saved —
	// should not have to learn what sizes this archive happens to keep.
	mux.HandleFunc("GET /v1/assets/{id}/thumb/{size}", allow(s.handleThumbSized))
	mux.HandleFunc("GET /v1/assets/{id}/preview", allow(s.handlePreview))
	mux.HandleFunc("GET /v1/assets/{id}/playback", allow(s.handlePlayback))

	// The same two renditions of a Snapchat memory without the caption layer
	// the ones above compose in. They are variants rather than the default
	// because the composite is the picture: the layer alone was never shown by
	// Snapchat and the photograph alone was never sent by anyone.
	mux.HandleFunc("GET /v1/assets/{id}/preview/plain", allow(s.handlePreviewPlain))
	mux.HandleFunc("GET /v1/assets/{id}/playback/plain", allow(s.handlePlaybackPlain))

	// A Live Photo's motion, addressed by the still's id. The stored sizes are
	// the grid's, which plays it on hover; the viewer's press-and-hold gets its
	// own rendition, rendered per request.
	mux.HandleFunc("GET /v1/assets/{id}/live/thumb", allow(s.handleLiveThumb))
	mux.HandleFunc("GET /v1/assets/{id}/live/thumb/{size}", allow(s.handleLiveThumbSized))
	mux.HandleFunc("GET /v1/assets/{id}/live/preview", allow(s.handleLivePreview))

	// Guarded with the rest: a failed job carries a filename and an error string,
	// which is archive content by another name.
	// What the archive thinks is duplicated, and what it has already put back
	// together. Reads of archive content — a group is a list of filenames and
	// thumbnails — so they sit with the rest of the gallery's reads rather than
	// with the writes below.
	mux.HandleFunc("GET /v1/merges", allow(s.handleMergeCounts))
	mux.HandleFunc("GET /v1/merges/groups", allow(s.handleMergeGroups))

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
