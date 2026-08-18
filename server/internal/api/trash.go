package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/purge"
)

// maxSelectionBody is generous for a few hundred ids or a thousand runs, and
// small enough that a confused client cannot buffer the process to death.
const maxSelectionBody = 1 << 20

// maxSelectionIDs bounds the exact half of a selection. Anything larger is a
// range: a client holding ten thousand ids got them from a timeline, and the
// timeline can say the same thing in one interval.
const maxSelectionIDs = 1000

// selectionRequest is how every operation in this file names what it applies to.
//
// Two ways of saying it, and both may be used at once. Ids are what the gallery
// sends for a tile it is actually holding, and they are exact. Ranges are
// positions in the filtered timeline the client is looking at, which is the only
// way to name a selection it has never fetched — see db.Selection.
//
// The filter travels with the ranges because a position means nothing without
// it: index 2 is a different photograph in an album than in the library, and the
// grid that made the selection was drawn from the album's day table.
type selectionRequest struct {
	IDs    []string   `json:"ids,omitempty"`
	Ranges []db.Range `json:"ranges,omitempty"`

	Album    string `json:"album,omitempty"`
	Person   string `json:"person,omitempty"`
	Category string `json:"category,omitempty"`
}

// readSelection reads the body, or answers the client and reports that it did.
//
// Which half of the archive the positions are counted in is not the request's
// to choose: it comes from the endpoint. A delete resolves in the library and a
// restore in the trash, so neither can be talked into reaching across.
func readSelection(w http.ResponseWriter, r *http.Request) (db.Selection, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSelectionBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body: "+err.Error())
		return db.Selection{}, false
	}
	return decodeSelection(w, body)
}

// trashResponse is what an operation that can be undone hands back.
//
// The batch is the undo. Not the ids it deleted: a selection can be the whole
// library, and the point of naming it by position was not to have to enumerate
// it. The batch names the same rows an hour later, after the timeline has been
// redrawn and every position in it has moved.
type trashResponse struct {
	Batch   string `json:"batch"`
	Deleted int    `json:"deleted"`
	// Albums is how many album rows went with the photos. Only ever set by the
	// album endpoints; omitted everywhere else.
	Albums int `json:"albums,omitempty"`
}

// handleTrash moves a selection into Recently Deleted.
//
// Nothing is destroyed and nothing is moved. See db.Trash — the whole operation
// is one column, which is what makes it safe enough to put behind a keystroke.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	sel, ok := readSelection(w, r)
	if !ok {
		return
	}

	result, err := s.Store.Trash(r.Context(), sel)
	if err != nil {
		s.writeSelectionError(w, err, "move a selection to the trash")
		return
	}
	writeJSON(w, http.StatusOK, trashResponse{Batch: result.Batch, Deleted: result.Count})
}

// restoreRequest names a restore either by the operation that caused it or by
// what to pull out of the trash.
type restoreRequest struct {
	Batch string `json:"batch,omitempty"`
}

type restoreResponse struct {
	Restored int `json:"restored"`
	Albums   int `json:"albums,omitempty"`
}

// handleRestore puts things back.
//
// Two callers with two different questions. The toast's Undo knows the batch
// and nothing else — by the time it is clicked the selection is gone and the
// grid has been redrawn — so it names the operation. The trash page knows what
// is selected in front of it, so it names positions in the trash. Both land
// here because they are the same write.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	// Buffered once so the body can be read as a batch and, failing that, as a
	// selection.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSelectionBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body: "+err.Error())
		return
	}

	var byBatch restoreRequest
	if err := json.Unmarshal(body, &byBatch); err == nil && byBatch.Batch != "" {
		assets, albums, err := s.Store.RestoreBatch(r.Context(), byBatch.Batch)
		if err != nil {
			if isBadUUID(err) {
				writeError(w, http.StatusBadRequest, "malformed batch")
				return
			}
			s.logger().Error("restore a batch", "error", err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, restoreResponse{Restored: assets, Albums: albums})
		return
	}

	sel, ok := decodeSelection(w, body)
	if !ok {
		return
	}
	restored, err := s.Store.Restore(r.Context(), sel)
	if err != nil {
		s.writeSelectionError(w, err, "restore a selection")
		return
	}
	writeJSON(w, http.StatusOK, restoreResponse{Restored: restored})
}

type purgeResponse struct {
	Purged int   `json:"purged"`
	Bytes  int64 `json:"bytes"`
}

// handlePurge destroys a selection outright.
//
// The one endpoint here with no undo, and the only one in the whole server that
// removes an original. It can only reach things already in the trash — see
// db.Purge — so getting here takes two deliberate acts rather than one.
func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	sel, ok := readSelection(w, r)
	if !ok {
		return
	}

	result, err := purge.Selection(r.Context(), s.purger(), sel)
	if err != nil {
		s.writeSelectionError(w, err, "purge a selection")
		return
	}
	s.logger().Info("purged a selection from the trash",
		"items", result.Items, "rows", result.Rows, "bytes", result.Bytes)
	writeJSON(w, http.StatusOK, purgeResponse{Purged: result.Items, Bytes: result.Bytes})
}

// handleDeleteAlbum removes an album, and — with ?photos=true — everything in
// it.
//
// The default is the harmless one on purpose. An album is a grouping an import
// produced, and dropping it leaves every photograph exactly where it was; the
// destructive reading of "delete this album" is available, but it has to be
// asked for.
func (s *Server) handleDeleteAlbum(w http.ResponseWriter, r *http.Request) {
	photos := truthy(r.URL.Query().Get("photos"))

	result, err := s.Store.DeleteAlbum(r.Context(), r.PathValue("id"), photos)
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such album")
		return
	case err != nil:
		if isBadUUID(err) {
			writeError(w, http.StatusNotFound, "no such album")
			return
		}
		s.logger().Error("delete album", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, trashResponse{
		Batch: result.Batch, Deleted: result.Count, Albums: 1,
	})
}

// purger assembles the four places one asset lives. Built per call rather than
// held on the Server because it is three pointers and a logger, and a second
// field holding the same ones is a second thing to keep in step.
func (s *Server) purger() purge.Deps {
	return purge.Deps{
		Store:       s.Store,
		Blobs:       s.Blobs,
		Derivatives: s.Derivatives,
		Manifest:    s.Manifest,
		Log:         s.logger(),
	}
}

// writeSelectionError answers for the operations that share a selection. All of
// them can fail on what the request named rather than on the archive, and those
// are the client's to fix.
func (s *Server) writeSelectionError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, db.ErrEmptySelection):
		writeError(w, http.StatusBadRequest, "the selection names no items")
	case errors.Is(err, db.ErrUnknownCategory):
		writeError(w, http.StatusBadRequest, "unknown category")
	case isBadUUID(err):
		writeError(w, http.StatusBadRequest, "malformed id")
	default:
		s.logger().Error(what, "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
	}
}

// decodeSelection reads a selection out of a body already in hand, which is
// what the restore endpoint needs: it has to look at the same bytes twice,
// once as a batch and once as a selection, and a request body is a stream only
// one decoder gets.
func decodeSelection(w http.ResponseWriter, body []byte) (db.Selection, bool) {
	var req selectionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return db.Selection{}, false
	}
	if len(req.IDs) > maxSelectionIDs {
		writeError(w, http.StatusBadRequest, "too many ids; name a range instead")
		return db.Selection{}, false
	}
	if len(req.IDs) == 0 && len(req.Ranges) == 0 {
		writeError(w, http.StatusBadRequest, "name what to act on with ids or ranges")
		return db.Selection{}, false
	}

	filter := db.TimelineFilter{AlbumID: req.Album, Person: req.Person, Category: req.Category}
	if named(filter) > 1 {
		writeError(w, http.StatusBadRequest, "name at most one of album, person, category")
		return db.Selection{}, false
	}
	return db.Selection{IDs: req.IDs, Ranges: req.Ranges, Filter: filter}, true
}

// truthy reads the query-string spelling of yes.
func truthy(v string) bool {
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
