package api

import (
	"net/http"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
)

// Albums inside a bucket: making one, and moving hidden photographs in and out.
//
// Three endpoints that look exactly like the library's three and share almost
// none of their implementation, which is the honest shape rather than an
// oversight. A hidden photograph's album membership is inside its sealed
// document — that is where CommitVault put it, and it is what a restore reads
// back — so there is no album_assets row to insert and no query that could
// insert one. Adding a hidden photograph to a hidden album means opening its
// document, adding a line, and sealing it again.
//
// Two consequences fall straight out of that and are worth stating rather than
// discovering:
//
//   - These need the password. Everything that puts something *into* the vault
//     works while it is locked; rearranging what is already in there is reading
//     it, and reading needs the key. Creating an empty album in a bucket is the
//     one exception — it writes a row and touches no document — but it is
//     behind the lock anyway, because the page it is reached from is.
//   - An album and the photographs going into it must be in the same bucket.
//     The endpoint's own {bucket} decides, so there is no request that can put
//     an archived photograph into a hidden album by naming both.

// handleVaultCreateAlbum makes an album inside a bucket, optionally with a
// selection of what is already in there.
//
// The album row is not encrypted and never has been — see migration 0012. What
// makes it an archived album is one column, and the photographs it names are
// named from inside their own sealed documents.
func (s *Server) handleVaultCreateAlbum(w http.ResponseWriter, r *http.Request) {
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if !s.vaultReady(w) {
		return
	}
	req, ok := readAlbumRequest(w, r)
	if !ok {
		return
	}

	album, err := s.Store.CreateAlbum(r.Context(), db.NewAlbum{
		Title: req.Title, Description: req.Description, Vault: bucket,
	})
	if err != nil {
		s.writeAlbumError(w, err, "create an album in the vault")
		return
	}

	response := albumResponse{Album: album}
	if sel, has := req.selection(); has {
		ids, ok := s.vaultSelection(w, r, bucket, sel)
		if !ok {
			return
		}
		added, err := s.Vault.SetAlbum(r.Context(), bucket, ids, refOf(album), true)
		if err != nil {
			// The album is already made and already on screen. Reported rather
			// than rolled back, for the same reason the library's create is:
			// undoing it would take away the half that worked.
			s.writeVaultError(w, err, "fill a new album in the vault")
			return
		}
		response.Added = added
		response.Count = added
	}
	s.logger().Info("created an album in the vault",
		"bucket", bucket, "album", album.ID, "items", response.Added)
	writeJSON(w, http.StatusCreated, response)
}

// handleVaultAlbumAdd puts hidden photographs into a hidden album.
func (s *Server) handleVaultAlbumAdd(w http.ResponseWriter, r *http.Request) {
	s.setVaultAlbum(w, r, true)
}

// handleVaultAlbumRemove takes them back out.
//
// The photographs stay hidden. What is removed is one line of one sealed
// document, which is why this is the vault's counterpart to the library's
// remove and not to anything destructive.
func (s *Server) handleVaultAlbumRemove(w http.ResponseWriter, r *http.Request) {
	s.setVaultAlbum(w, r, false)
}

// setVaultAlbum is both halves, because they are one write with a boolean: open
// every named document, add or drop the reference, seal it again.
func (s *Server) setVaultAlbum(w http.ResponseWriter, r *http.Request, member bool) {
	bucket, ok := bucketOf(w, r)
	if !ok {
		return
	}
	if !s.vaultReady(w) {
		return
	}

	album, home, err := s.Store.AlbumHome(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAlbumError(w, err, "look up an album in the vault")
		return
	}
	if home != bucket {
		writeError(w, http.StatusConflict, "that album is not in this vault")
		return
	}

	sel, ok := readSelection(w, r)
	if !ok {
		return
	}
	ids, ok := s.vaultSelection(w, r, bucket, sel)
	if !ok {
		return
	}

	changed, err := s.Vault.SetAlbum(r.Context(), bucket, ids, refOf(album), member)
	if err != nil {
		s.writeVaultError(w, err, "change what is in an album in the vault")
		return
	}
	if member {
		writeJSON(w, http.StatusOK, membershipResponse{Added: changed})
		return
	}
	writeJSON(w, http.StatusOK, membershipResponse{Removed: changed})
}

// vaultSelection turns a selection into the ids it names inside a bucket.
//
// Positions in the vault are positions in an index this server holds in memory
// rather than rows a query could number, so they are resolved here — against
// the same index that drew the grid the selection was made in. The collection
// the positions were counted in travels in the body exactly as it does for the
// library, so a client makes one shape of request either way.
func (s *Server) vaultSelection(w http.ResponseWriter, r *http.Request, bucket string, sel db.Selection) ([]string, bool) {
	ids := append([]string(nil), sel.IDs...)
	if len(sel.Ranges) == 0 {
		if len(ids) == 0 {
			writeError(w, http.StatusBadRequest, "the selection names no items")
			return nil, false
		}
		return ids, true
	}

	index, ok := s.vaultIndex(w, r, bucket)
	if !ok {
		return nil, false
	}
	items := index.Select(vault.Filter{
		AlbumID: sel.Filter.AlbumID, Person: sel.Filter.Person, Category: sel.Filter.Category,
	})
	for _, run := range sel.Ranges {
		for i := max(run.Start, 0); i < run.End && i < len(items); i++ {
			ids = append(ids, items[i].ID())
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "the selection names no items")
		return nil, false
	}
	return ids, true
}

// refOf is the album as a sealed document names it: the id a restore finds it
// by, and the title an unlocked vault draws it with.
func refOf(album db.Album) vault.AlbumRef {
	return vault.AlbumRef{ID: album.ID, Title: album.Title, Source: album.Source}
}
