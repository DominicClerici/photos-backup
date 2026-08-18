package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
)

// Making albums, and moving photographs in and out of them.
//
// The read side of albums has always been here — the collections page lists
// them, the timeline filters by one — and until now the only thing that could
// *write* one was an import. These are the three writes that were missing, and
// they are deliberately the same three shapes everything else in the gallery
// uses: a selection goes in the body, positions are counted in the timeline the
// client is looking at, and the answer says how many things moved.
//
// The vault's half of the same three lives in vaultalbums.go, because it is a
// different write rather than the same write with a flag on it. See there.

// maxAlbumTitle and maxAlbumDescription are floors under a text field rather
// than a policy about names. An album can be called almost anything; what these
// refuse is the megabyte somebody's script pasted into the wrong field.
const (
	maxAlbumTitle       = 200
	maxAlbumDescription = 2000
)

// albumRequest is "make an album", optionally with something to put in it.
//
// The selection is optional and the two halves are one request on purpose. The
// common way to make an album is from a selection — right-click, Add to album,
// Create "Iceland" — and splitting that into create-then-add would leave a
// failure mode where the album exists and is empty, which somebody then has to
// notice and clean up.
type albumRequest struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`

	IDs      []string   `json:"ids,omitempty"`
	Ranges   []db.Range `json:"ranges,omitempty"`
	Album    string     `json:"album,omitempty"`
	Person   string     `json:"person,omitempty"`
	Category string     `json:"category,omitempty"`
}

// selection is the "what to put in it" half, or false when there is none.
func (req albumRequest) selection() (db.Selection, bool) {
	if len(req.IDs) == 0 && len(req.Ranges) == 0 {
		return db.Selection{}, false
	}
	return db.Selection{
		IDs:    req.IDs,
		Ranges: req.Ranges,
		Filter: db.TimelineFilter{AlbumID: req.Album, Person: req.Person, Category: req.Category},
	}, true
}

// albumResponse is the album that was just made, plus what went into it.
type albumResponse struct {
	db.Album
	// Added is how many photographs the same request put in. Zero, and omitted,
	// for an album made from the collections page with nothing selected.
	Added int `json:"added,omitempty"`
}

// membershipResponse is what adding to or removing from an album hands back.
//
// A count of rows that actually changed rather than of rows named, so a client
// can tell "added forty" from "all forty were already in there" and say the
// true one. There is no batch and no undo token: nothing has been destroyed,
// the photographs have not moved, and putting them back is the same gesture
// again.
type membershipResponse struct {
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
}

// readAlbumRequest reads and checks the name.
func readAlbumRequest(w http.ResponseWriter, r *http.Request) (albumRequest, bool) {
	var req albumRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSelectionBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return req, false
	}

	// Trimmed rather than rejected for surrounding space: somebody who typed a
	// trailing space meant the name without it, and an album called "Iceland "
	// that sorts beside "Iceland" is nobody's intention.
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	switch {
	case req.Title == "":
		writeError(w, http.StatusBadRequest, "an album needs a name")
		return req, false
	case len(req.Title) > maxAlbumTitle:
		writeError(w, http.StatusBadRequest, "that name is too long")
		return req, false
	case len(req.Description) > maxAlbumDescription:
		writeError(w, http.StatusBadRequest, "that description is too long")
		return req, false
	}
	return req, true
}

// handleAlbumList is the albums alone.
//
// The collections page reads them as part of its one round trip and does not
// need this. The "Add to album" menu does: it opens over a grid somebody is
// already looking at, and asking for the people and the category covers in
// order to draw a list of album names would be three queries for one.
func (s *Server) handleAlbumList(w http.ResponseWriter, r *http.Request) {
	albums, err := s.Store.Albums(r.Context())
	if err != nil {
		s.logger().Error("list albums", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": albums})
}

// handleCreateAlbum makes one, and fills it if the request said what with.
func (s *Server) handleCreateAlbum(w http.ResponseWriter, r *http.Request) {
	req, ok := readAlbumRequest(w, r)
	if !ok {
		return
	}

	album, err := s.Store.CreateAlbum(r.Context(), db.NewAlbum{
		Title: req.Title, Description: req.Description,
	})
	if err != nil {
		s.writeAlbumError(w, err, "create an album")
		return
	}

	response := albumResponse{Album: album}
	if sel, has := req.selection(); has {
		// An album that was made and then could not be filled is still an
		// album, and it is on screen by the time this fails. Reported rather
		// than rolled back: undoing the creation would take away the one thing
		// that did work, and the client's own retry is one gesture.
		added, err := s.Store.AddToAlbum(r.Context(), album.ID, sel)
		if err != nil {
			s.writeSelectionError(w, err, "fill a new album")
			return
		}
		response.Added = added
		response.Count = added
	}
	s.logger().Info("created an album", "album", album.ID, "items", response.Added)
	writeJSON(w, http.StatusCreated, response)
}

// handleAlbumAdd puts a selection into an existing album.
func (s *Server) handleAlbumAdd(w http.ResponseWriter, r *http.Request) {
	id, ok := s.libraryAlbum(w, r)
	if !ok {
		return
	}
	sel, ok := readSelection(w, r)
	if !ok {
		return
	}

	added, err := s.Store.AddToAlbum(r.Context(), id, sel)
	if err != nil {
		s.writeSelectionError(w, err, "add a selection to an album")
		return
	}
	writeJSON(w, http.StatusOK, membershipResponse{Added: added})
}

// handleAlbumRemove takes a selection back out of one.
//
// Not a delete and nowhere near the trash: what is removed is the membership,
// and every photograph stays in the library, in its other albums, and in the
// timeline. Which is why there is no batch to undo — putting them back is the
// same menu again.
func (s *Server) handleAlbumRemove(w http.ResponseWriter, r *http.Request) {
	id, ok := s.libraryAlbum(w, r)
	if !ok {
		return
	}
	sel, ok := readSelection(w, r)
	if !ok {
		return
	}

	removed, err := s.Store.RemoveFromAlbum(r.Context(), id, sel)
	if err != nil {
		s.writeSelectionError(w, err, "remove a selection from an album")
		return
	}
	writeJSON(w, http.StatusOK, membershipResponse{Removed: removed})
}

// handleAssetAlbums is which albums hold one photograph.
//
// It exists for the ticks in the "Add to album" menu, which is why it is ids
// rather than the titles the viewer's panel shows: a menu row is a row for an
// album, and two imports can each have contributed a "Favorites".
//
// A hidden photograph answers from its sealed document, which means this is one
// of the handful of endpoints where the two halves of the archive read from
// different places — and it needs the password, because that document is the
// only thing that knows.
func (s *Server) handleAssetAlbums(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	if asset.Vault != "" {
		if !s.vaultReady(w) {
			return
		}
		item, err := s.Vault.Item(r.Context(), asset.ID)
		if err != nil {
			s.writeVaultError(w, err, "read the albums of a hidden photo")
			return
		}
		ids := make([]string, 0, len(item.Doc.Albums))
		for _, ref := range item.Doc.Albums {
			ids = append(ids, ref.ID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"albums": ids})
		return
	}

	ids, err := s.Store.AlbumIDsOf(r.Context(), asset.ID)
	if err != nil {
		s.logger().Error("read the albums of an asset", "error", err, "asset", asset.ID)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": ids})
}

// libraryAlbum reads the {id} path value and refuses an album that is not in
// the library.
//
// The refusal is the point. An album in the Archive holds its membership inside
// sealed documents rather than in album_assets, so writing to it through this
// endpoint would silently do nothing — or, worse, write a plaintext membership
// row naming a hidden photograph.
func (s *Server) libraryAlbum(w http.ResponseWriter, r *http.Request) (string, bool) {
	album, bucket, err := s.Store.AlbumHome(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAlbumError(w, err, "look up an album")
		return "", false
	}
	if bucket != "" {
		writeError(w, http.StatusConflict, "that album is in the vault")
		return "", false
	}
	return album.ID, true
}

// writeAlbumError maps the failures the album endpoints share onto statuses.
func (s *Server) writeAlbumError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, db.ErrDuplicateAlbum):
		// 409 rather than 400: the request was well-formed and the archive is
		// the reason it cannot happen. The gallery turns exactly this into the
		// line under the name field.
		writeError(w, http.StatusConflict, "an album with that name already exists")
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such album")
	case errors.Is(err, db.ErrBadBucket):
		writeError(w, http.StatusNotFound, "no such vault")
	case errors.Is(err, vault.ErrLocked):
		writeError(w, http.StatusLocked, "the vault is locked")
	case isBadUUID(err):
		writeError(w, http.StatusNotFound, "no such album")
	default:
		s.logger().Error(what, "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
	}
}
