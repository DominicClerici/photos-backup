package api

import (
	"errors"
	"net/http"
	"os"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/vault"
)

// The media endpoints, for the half of the archive that is encrypted.
//
// Everything here exists because of one asymmetry: a vaulted asset's row knows
// its digest and nothing else. The path a rendition lives at is a function of
// the digest, so a thumbnail can be served by decrypting a file — but the
// content type, the filename and the extension are all in the sealed document,
// so the original cannot.
//
// The stored renditions are decrypted straight into the response, chunk by
// chunk, and never touch the disk in the clear. The three that are rendered on
// demand cannot be: ImageMagick and ffmpeg want a seekable path. See
// vault.Materialize for what that costs and why it is confined to those three.

// vaultReady answers the two ways this can be unavailable before a handler
// commits to anything: a server with no vault wired up, and a locked one.
func (s *Server) vaultReady(w http.ResponseWriter) bool {
	if s.Vault == nil {
		writeError(w, http.StatusNotFound, "this server has no vault")
		return false
	}
	if !s.Vault.Keeper.Unlocked() {
		// 423 rather than 401 or 403. There is no credential to present on this
		// request and nothing about the caller is being refused — the archive
		// itself is shut, and the gallery turns exactly this status into the
		// password prompt.
		writeError(w, http.StatusLocked, "the vault is locked")
		return false
	}
	return true
}

// serveVaultOriginal streams a decrypted original.
//
// ServeContent over the vault reader, which is why that reader is a Seeker: a
// Range request on a 4GB video decrypts the two chunks it covers rather than
// the file up to them.
func (s *Server) serveVaultOriginal(w http.ResponseWriter, r *http.Request, asset db.Asset) {
	if !s.vaultReady(w) {
		return
	}
	item, err := s.Vault.Item(r.Context(), asset.ID)
	if err != nil {
		s.vaultFailure(w, err, "load the sealed metadata", asset.ID)
		return
	}
	reader, err := s.Vault.OpenOriginal(asset.SHA256)
	if err != nil {
		s.vaultFailure(w, err, "open a sealed original", asset.ID)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", item.ContentType())
	w.Header().Set("Cache-Control", vaultCache)
	http.ServeContent(w, r, item.Filename(), asset.UploadedAt, reader)
}

// serveVaultDerivative streams a decrypted thumbnail, playback file or motion
// clip. The sealed document is not read at all: the path is the digest and the
// content type is the caller's, both of which are already known.
func (s *Server) serveVaultDerivative(w http.ResponseWriter, r *http.Request, asset db.Asset, suffix, contentType string) {
	if !s.vaultReady(w) {
		return
	}
	reader, err := s.Vault.OpenDerivative(asset.SHA256, suffix)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "derivative not generated yet")
			return
		}
		s.vaultFailure(w, err, "open a sealed derivative", asset.ID)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", vaultCache)
	http.ServeContent(w, r, asset.SHA256+suffix, asset.UploadedAt, reader)
}

// vaultCache is what a hidden photograph's renditions are cached as.
//
// Deliberately not `immutable`. The bytes are still a pure function of a digest
// and could not go stale, so the ordinary argument for caching them forever
// still holds — but a browser cache is a copy of the picture on the same disk,
// outside the vault, surviving the lock. `no-store` is the only header that
// says "when this is locked again, it is gone", and re-decrypting a 20KB
// thumbnail is a millisecond.
const vaultCache = "no-store"

// sourcePath is where a tool that needs a real file should read this asset.
//
// For the library that is the blob itself and the cleanup does nothing. For the
// vault it is a decrypted copy in the staging directory, and the caller must
// release it.
func (s *Server) sourcePath(w http.ResponseWriter, asset db.Asset) (string, func(), bool) {
	if asset.Vault == "" {
		return s.Blobs.Path(asset.SHA256, asset.Ext), func() {}, true
	}
	if !s.vaultReady(w) {
		return "", func() {}, false
	}
	path, release, err := s.Vault.Materialize(asset.SHA256, "")
	if err != nil {
		s.vaultFailure(w, err, "decrypt an original for rendering", asset.ID)
		return "", func() {}, false
	}
	return path, release, true
}

// assetPath is sourcePath for the one caller that is not holding a
// ResponseWriter — the Live Photo render, which runs inside the preview cache's
// loader and reports failure by returning an error.
func (s *Server) assetPath(asset db.Asset) (string, func(), error) {
	if asset.Vault == "" {
		return s.Blobs.Path(asset.SHA256, asset.Ext), func() {}, nil
	}
	if s.Vault == nil {
		return "", func() {}, vault.ErrLocked
	}
	return s.Vault.Materialize(asset.SHA256, "")
}

// vaultFailure maps the three ways a vault read goes wrong onto statuses that
// mean something: shut, absent, or genuinely broken.
func (s *Server) vaultFailure(w http.ResponseWriter, err error, what, assetID string) {
	switch {
	case errors.Is(err, vault.ErrLocked):
		writeError(w, http.StatusLocked, "the vault is locked")
	case errors.Is(err, db.ErrNotFound) || os.IsNotExist(err):
		writeError(w, http.StatusNotFound, "no such item in the vault")
	case errors.Is(err, vault.ErrCorrupt):
		// Worth a log line at error level rather than a warning. Content-
		// addressed ciphertext that will not authenticate is either disk rot or
		// somebody having been at the files, and both are things to find out
		// about on the day they happen.
		s.logger().Error("sealed data did not authenticate", "asset", assetID, "doing", what)
		writeError(w, http.StatusInternalServerError, "this item did not decrypt; the archive may be damaged")
	default:
		s.logger().Error("vault failure", "error", err, "asset", assetID, "doing", what)
		writeError(w, http.StatusInternalServerError, "could not "+what)
	}
}

// vaultDetail answers the viewer's information panel for a hidden photograph.
//
// The albums here are titles read out of the sealed document rather than rows
// looked up in the database, and that is not a shortcut: the membership was
// deleted when the photograph was hidden, so the document is the only thing
// that still knows. It is also the only thing that can still name an album
// which has since been deleted outright — the panel says where the photograph
// was, which is a true statement about the photograph either way.
func (s *Server) vaultDetail(w http.ResponseWriter, r *http.Request, asset db.Asset) {
	if !s.vaultReady(w) {
		return
	}
	item, err := s.Vault.Item(r.Context(), asset.ID)
	if err != nil {
		s.vaultFailure(w, err, "load the sealed metadata", asset.ID)
		return
	}

	d := item.Detail()
	writeJSON(w, http.StatusOK, assetDetail{
		ID:              asset.ID,
		Filename:        d.Filename,
		MediaKind:       d.MediaKind,
		SHA256:          d.SHA256,
		ByteSize:        d.ByteSize,
		Width:           d.Width,
		Height:          d.Height,
		DurationSeconds: d.DurationSeconds,
		TakenAt:         d.TakenAt,
		OffsetMinutes:   d.OffsetMinutes,
		ReportedAt:      d.ReportedAt,
		UploadedAt:      d.UploadedAt,
		CameraMake:      d.CameraMake,
		CameraModel:     d.CameraModel,
		Lens:            d.Lens,
		GPSLat:          d.GPSLat,
		GPSLon:          d.GPSLon,
		PlaceCity:       d.PlaceCity,
		PlaceAdmin1:     d.PlaceAdmin1,
		PlaceCountry:    d.PlaceCountry,
		Description:     d.Description,
		Favorite:        d.Favorite,
		Archived:        d.Archived,
		Albums:          d.Albums,
		People:          d.People,
		HasOverlay:      d.HasOverlay,
		State:           d.State,
		PlaybackState:   d.PlaybackState,
	})
}
