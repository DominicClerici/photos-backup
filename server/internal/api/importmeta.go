package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

// maxSidecarBody bounds one sidecar. Google's run to a few hundred bytes; a
// heavily tagged one with a long description and a crowd of people is still
// nowhere near this.
const maxSidecarBody = 256 << 10

type importMetadataRequest struct {
	// Source names the export format, which is what says how to read Sidecar.
	Source string `json:"source"`
	// Sidecar is the source's own JSON for this item, verbatim.
	Sidecar json.RawMessage `json:"sidecar"`
	// Albums are the albums the item belonged to. They come from the export's
	// directory layout rather than from the sidecar, so only the importer
	// walking that layout can supply them.
	Albums []albumRef `json:"albums"`
}

type albumRef struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// handleImportMetadata records what an export knew about an asset that the
// asset does not know about itself.
//
// Separate from the upload rather than folded into it because the two are about
// different things and fail differently. An upload moves bytes and must not be
// held up by a JSON parse; this moves a description of bytes that are already
// archived, and can be retried, corrected, or run again over a library that was
// imported before the parser understood people tags.
//
// It is idempotent, and applying it twice is not merely harmless but the
// expected case: a Takeout arrives as a stack of zips, an item can appear in
// both a dated folder and an album folder, and the way anyone recovers a
// half-finished import is to start it again.
func (s *Server) handleImportMetadata(w http.ResponseWriter, r *http.Request, _ devices.Device) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	var req importMetadataRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSidecarBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	albums := make([]db.AlbumRef, 0, len(req.Albums))
	for _, album := range req.Albums {
		albums = append(albums, db.AlbumRef{Title: album.Title, Description: album.Description})
	}
	meta, err := db.ImportMetadataFrom(req.Source, req.Sidecar, albums)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The log before the database, the same ordering every write in this server
	// uses: a line that outlives a lost database is worth more than a row that
	// outlives a lost line, and only one of the two can be written first.
	if s.Manifest != nil {
		entry := manifest.Entry{
			Type:          manifest.KindMetadata,
			SHA256:        asset.SHA256,
			ImportSource:  req.Source,
			ImportSidecar: req.Sidecar,
			StoredAt:      time.Now().UTC(),
		}
		for _, album := range req.Albums {
			entry.ImportAlbums = append(entry.ImportAlbums,
				manifest.AlbumRef{Title: album.Title, Description: album.Description})
		}
		if err := s.Manifest.Append(entry); err != nil {
			s.logger().Error("append import metadata to manifest", "error", err, "sha256", asset.SHA256)
			writeError(w, http.StatusInternalServerError, "could not record the metadata")
			return
		}
	}

	if err := s.Store.ApplyImportMetadata(r.Context(), asset.ID, meta); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no such asset")
			return
		}
		s.logger().Error("apply import metadata", "error", err, "asset", asset.ID)
		writeError(w, http.StatusServiceUnavailable, "metadata recorded but not indexed; retry to reconcile")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
