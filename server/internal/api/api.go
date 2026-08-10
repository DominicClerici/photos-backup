// Package api serves the upload endpoint and the bare gallery.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/blobstore"
	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/manifest"
)

type Server struct {
	Store     *db.Store
	Blobs     *blobstore.Store
	Manifest  *manifest.Log
	Converter *derive.Converter
	Log       *slog.Logger
}

type uploadResponse struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	Duplicate bool   `json:"duplicate"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sync/check", s.handleSyncCheck)
	mux.HandleFunc("POST /v1/assets", s.handleUpload)
	mux.HandleFunc("GET /v1/assets/{id}/original", s.handleOriginal)
	mux.HandleFunc("GET /v1/assets/{id}/web", s.handleWeb)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleGallery)
	return mux
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUpload commits an original in the order that makes a crash survivable:
// blob first, then the manifest line, then the database row. Anything lost
// after the blob lands is recoverable by re-uploading the same bytes.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	meta, err := parseUploadHeaders(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ext := strings.ToLower(filepath.Ext(meta.filename))
	res, err := s.Blobs.Put(r.Body, ext, blobstore.Expected{MD5: meta.md5, Size: meta.size})
	switch {
	case errors.Is(err, blobstore.ErrChecksumMismatch), errors.Is(err, blobstore.ErrSizeMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.logger().Error("store blob", "error", err)
		writeError(w, http.StatusInternalServerError, "could not store upload")
		return
	}

	if res.Created {
		entry := manifest.Entry{
			SHA256:     res.SHA256,
			MD5:        res.MD5,
			Size:       res.Size,
			Filename:   meta.filename,
			CapturedAt: meta.capturedAt,
			ModifiedAt: meta.modifiedAt,
			DeviceID:   meta.deviceID,
			LocalID:    meta.localID,
			StoredAt:   time.Now().UTC(),
		}
		if err := s.Manifest.Append(entry); err != nil {
			s.logger().Error("append manifest", "error", err, "sha256", res.SHA256)
			writeError(w, http.StatusInternalServerError, "could not record upload")
			return
		}
	}

	id, inserted, err := s.Store.RecordAsset(r.Context(), db.Asset{
		SHA256:           res.SHA256,
		MD5:              res.MD5,
		ByteSize:         res.Size,
		OriginalFilename: meta.filename,
		Ext:              ext,
		ContentType:      contentTypeFor(ext),
		CapturedAt:       meta.capturedAt,
		ModifiedAt:       meta.modifiedAt,
		DeviceID:         meta.deviceID,
		LocalID:          meta.localID,
	})
	if err != nil {
		s.logger().Error("record asset", "error", err, "sha256", res.SHA256)
		writeError(w, http.StatusServiceUnavailable, "upload stored but not indexed; retry to reconcile")
		return
	}

	writeJSON(w, http.StatusCreated, uploadResponse{
		ID:        id,
		SHA256:    res.SHA256,
		Duplicate: !inserted,
	})
}

func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	f, err := s.Blobs.Open(asset.SHA256, asset.Ext)
	if err != nil {
		s.logger().Error("open blob", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "blob missing from disk")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", asset.ContentType)
	http.ServeContent(w, r, asset.OriginalFilename, asset.UploadedAt, f)
}

// handleWeb renders on demand and buffers the result, so a conversion failure
// still produces an honest status code instead of a truncated image.
func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	var buf bytes.Buffer
	if err := s.Converter.ToWebP(r.Context(), s.Blobs.Path(asset.SHA256, asset.Ext), &buf); err != nil {
		s.logger().Error("convert to webp", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if _, err := buf.WriteTo(w); err != nil {
		s.logger().Error("write webp", "error", err)
	}
}

func (s *Server) handleGallery(w http.ResponseWriter, r *http.Request) {
	assets, err := s.Store.RecentAssets(r.Context(), 200)
	if err != nil {
		s.logger().Error("list assets", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := galleryTemplate.Execute(w, assets); err != nil {
		s.logger().Error("render gallery", "error", err)
	}
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (db.Asset, bool) {
	asset, err := s.Store.Asset(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such asset")
		return db.Asset{}, false
	case err != nil:
		s.logger().Error("load asset", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return db.Asset{}, false
	}
	return asset, true
}

type uploadMeta struct {
	filename string
	md5      string
	size     int64
	// capturedAt is when the photo was taken; modifiedAt is when the local asset
	// last changed. The second one is what tells a later sync/check that an
	// edited photo needs re-examining even though its local id never changed.
	capturedAt *time.Time
	modifiedAt *time.Time
	deviceID   string
	localID    string
}

func parseUploadHeaders(r *http.Request) (uploadMeta, error) {
	var m uploadMeta
	var err error

	if m.filename, err = requiredHeader(r, "X-Photo-Filename"); err != nil {
		return m, err
	}
	if m.md5, err = requiredHeader(r, "X-Photo-Md5"); err != nil {
		return m, err
	}
	if m.deviceID, err = requiredHeader(r, "X-Photo-Device-Id"); err != nil {
		return m, err
	}
	if m.localID, err = requiredHeader(r, "X-Photo-Local-Id"); err != nil {
		return m, err
	}

	rawSize, err := requiredHeader(r, "X-Photo-Size")
	if err != nil {
		return m, err
	}
	if m.size, err = strconv.ParseInt(rawSize, 10, 64); err != nil {
		return m, fmt.Errorf("X-Photo-Size is not a number: %q", rawSize)
	}
	if m.size < 0 {
		return m, fmt.Errorf("X-Photo-Size is negative: %d", m.size)
	}

	if m.capturedAt, err = optionalTimeHeader(r, "X-Photo-Captured-At"); err != nil {
		return m, err
	}
	if m.modifiedAt, err = optionalTimeHeader(r, "X-Photo-Modified-At"); err != nil {
		return m, err
	}
	return m, nil
}

func optionalTimeHeader(r *http.Request, name string) (*time.Time, error) {
	raw := strings.TrimSpace(r.Header.Get(name))
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not RFC3339: %q", name, raw)
	}
	t = t.UTC()
	return &t, nil
}

func requiredHeader(r *http.Request, name string) (string, error) {
	v := strings.TrimSpace(r.Header.Get(name))
	if v == "" {
		return "", fmt.Errorf("missing required header %s", name)
	}
	return v, nil
}

func contentTypeFor(ext string) string {
	switch strings.ToLower(ext) {
	case ".heic", ".heif":
		return "image/heic"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

var galleryTemplate = template.Must(template.New("gallery").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>photobackup</title>
<style>
  body { background:#111; color:#eee; font:14px/1.5 system-ui, sans-serif; margin:0; padding:24px; }
  h1 { font-size:16px; font-weight:600; margin:0 0 20px; }
  .grid { display:grid; grid-template-columns:repeat(auto-fill, minmax(220px, 1fr)); gap:16px; }
  figure { margin:0; }
  img { width:100%; aspect-ratio:1; object-fit:cover; background:#222; border-radius:6px; display:block; }
  figcaption { color:#888; font-size:12px; margin-top:6px; overflow-wrap:anywhere; }
  .empty { color:#888; }
</style>
<h1>photobackup — {{len .}} archived</h1>
{{if .}}
<div class="grid">
  {{range .}}
  <figure>
    <a href="/v1/assets/{{.ID}}/original"><img src="/v1/assets/{{.ID}}/web" alt="{{.OriginalFilename}}" loading="lazy"></a>
    <figcaption>{{.OriginalFilename}}<br>{{.ByteSize}} bytes</figcaption>
  </figure>
  {{end}}
</div>
{{else}}
<p class="empty">Nothing uploaded yet.</p>
{{end}}
`))
