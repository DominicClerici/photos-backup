package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

type importOrphanRequest struct {
	Source  string          `json:"source"`
	Kind    string          `json:"kind"`
	Locator string          `json:"locator"`
	AssetID string          `json:"assetId"`
	Sidecar json.RawMessage `json:"sidecar"`
	Albums  []albumRef      `json:"albums"`
	Reason  string          `json:"reason"`
}

// handleImportOrphan records something an import read and could not attach.
//
// It is deliberately not part of import-metadata. That endpoint applies what a
// source knew to an asset; this one admits that something could not be applied,
// and the two have opposite failure modes — applying the wrong sidecar to the
// wrong photograph is worse than losing it, so a match this server is not sure
// about must not be guessed at here.
//
// Idempotent, like every other write on the import path: a re-run re-reads the
// same unmatched sidecars and must produce the same rows.
func (s *Server) handleImportOrphan(w http.ResponseWriter, r *http.Request, _ devices.Device) {
	var req importOrphanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSidecarBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	orphan := db.ImportOrphan{
		Source:  req.Source,
		Kind:    req.Kind,
		Locator: req.Locator,
		AssetID: req.AssetID,
		Sidecar: req.Sidecar,
		Reason:  req.Reason,
	}
	for _, album := range req.Albums {
		orphan.Albums = append(orphan.Albums,
			db.AlbumRef{Title: album.Title, Description: album.Description})
	}
	if err := orphan.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The log before the database, the same ordering every write in this server
	// uses — and more load-bearing here than anywhere else: an unmatched
	// sidecar is the only surviving copy of what an export knew about a
	// photograph, and the export is usually deleted the week after the import.
	if s.Manifest != nil {
		entry := manifest.Entry{
			Type:          manifest.KindImportOrphan,
			OrphanKind:    req.Kind,
			Locator:       req.Locator,
			OrphanReason:  req.Reason,
			ImportSource:  req.Source,
			ImportSidecar: req.Sidecar,
			StoredAt:      time.Now().UTC(),
		}
		for _, album := range req.Albums {
			entry.ImportAlbums = append(entry.ImportAlbums,
				manifest.AlbumRef{Title: album.Title, Description: album.Description})
		}
		if err := s.Manifest.Append(entry); err != nil {
			s.logger().Error("append import orphan to manifest", "error", err, "locator", req.Locator)
			writeError(w, http.StatusInternalServerError, "could not record the orphan")
			return
		}
	}

	if err := s.Store.RecordImportOrphan(r.Context(), orphan); err != nil {
		s.logger().Error("record import orphan", "error", err, "locator", req.Locator)
		writeError(w, http.StatusServiceUnavailable, "orphan recorded but not indexed; retry to reconcile")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
