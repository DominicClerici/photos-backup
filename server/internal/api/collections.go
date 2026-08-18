package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/dominicclerici/photos-backup/server/internal/db"
)

const (
	defaultPeopleLimit = 100
	maxPeopleLimit     = 500
)

// handleCollections serves the collections page in one request: the people, the
// albums, and the named slices of the library, each with a count and a cover.
//
// None of it paginates. Albums and people are counted in tens for this archive,
// not thousands, and a page that draws all three sections at once has nothing
// to do with three separate round trips but wait for them.
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	limit := defaultPeopleLimit
	if raw := r.URL.Query().Get("people"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPeopleLimit {
			writeError(w, http.StatusBadRequest, "people must be between 1 and 500")
			return
		}
		limit = n
	}

	collections, err := s.Store.Collections(r.Context(), limit)
	if err != nil {
		s.logger().Error("read collections", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	// The two vault rows carry a count only while the vault is open. Locked,
	// they say "Locked" and nothing else: how much somebody has hidden is a
	// fact about what they hid, and the promise this feature makes is that no
	// fact about it is readable without the password. Absent rather than zero,
	// so the page can tell "nothing in there" from "not saying".
	if s.Vault != nil && s.Vault.Keeper.Unlocked() {
		counts, err := s.Store.VaultCounts(r.Context())
		if err != nil {
			s.logger().Error("count the vault", "error", err)
		} else {
			collections.Vault = counts
		}
	}
	writeJSON(w, http.StatusOK, collections)
}

// handleAlbum is the heading of one album's page. Separate from the list so
// that opening an album does not cost a count of every other one.
func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	album, err := s.Store.AlbumByID(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such album")
		return
	case err != nil:
		if isBadUUID(err) {
			writeError(w, http.StatusNotFound, "no such album")
			return
		}
		s.logger().Error("load album", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, album)
}
