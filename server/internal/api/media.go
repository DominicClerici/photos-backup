package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derive"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
)

// Every byte this file serves is a pure function of a SHA-256, so it can be
// cached forever. Editing a photo on the phone produces different bytes, a
// different digest, and a different asset id — a URL's content can never
// change under a client.
const immutable = "public, max-age=31536000, immutable"

func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if asset.Vault != "" {
		s.serveVaultOriginal(w, r, asset)
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
	w.Header().Set("Cache-Control", immutable)
	w.Header().Set("ETag", etagFor(asset.SHA256, "original"))
	// ServeContent gives Range support for free, which is what lets a browser
	// scrub a video served straight from the archive.
	http.ServeContent(w, r, asset.OriginalFilename, asset.UploadedAt, f)
}

// handleThumb serves the base stored rendition, the one every client gets
// without asking for a size.
//
// It answers from the file rather than from derived_state, so a photo whose
// metadata job failed after the thumbnail was written still shows its picture.
// A missing file is a 404 the client turns into a placeholder tile.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derivstore.Thumb, "image/webp")
}

// handleThumbSized serves one named size of the stored rendition.
//
// A size the archive does not store is a 404 rather than a nearest match, and a
// size it stores but has not built yet is the same 404 the base route already
// answers with. Both matter because these URLs are immutable-cached: answering
// /thumb/512 with the 256px file would pin the wrong bytes in a browser's cache
// long after the real rendition existed. The gallery falls back on its own,
// which is a decision it can revisit on the next zoom.
func (s *Server) handleThumbSized(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	size, ok := thumbSize(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derivstore.ThumbSuffix(size), "image/webp")
}

// handlePlayback serves the browser-playable rendition of a video. For a
// Snapchat memory that is the composite: the caption is in the pixels, because
// nothing can lay a PNG over a playing video on the client.
func (s *Server) handlePlayback(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if asset.MediaKind != db.MediaVideo {
		writeError(w, http.StatusNotFound, "not a video")
		return
	}
	s.serveDerivative(w, r, asset, derivstore.Playback, "video/mp4")
}

// handlePlaybackPlain serves the same video without the caption layer burned
// in, which is the only way the viewer's toggle can show one of these.
//
// It exists only for videos that carry a layer. Anything else gets a 404 rather
// than a copy of the ordinary playback: there is no second rendition on disk,
// and answering with the first would pin the composite in a browser's immutable
// cache under the URL that promises not to be one.
func (s *Server) handlePlaybackPlain(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if asset.MediaKind != db.MediaVideo || asset.OverlayAssetID == nil {
		writeError(w, http.StatusNotFound, "this video has no overlay to leave out")
		return
	}
	s.serveDerivative(w, r, asset, derivstore.PlaybackPlain, "video/mp4")
}

// handleLiveThumb serves the base motion rendition the grid plays on hover.
func (s *Server) handleLiveThumb(w http.ResponseWriter, r *http.Request) {
	video, ok := s.livePair(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, video, derivstore.LiveThumb, "video/mp4")
}

// handleLiveThumbSized serves that motion at one of the other stored sizes, so
// the clip that plays on hover is the same size as the still it replaces at
// every zoom level.
func (s *Server) handleLiveThumbSized(w http.ResponseWriter, r *http.Request) {
	video, ok := s.livePair(w, r)
	if !ok {
		return
	}
	size, ok := thumbSize(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, video, derivstore.LiveSuffix(size), "video/mp4")
}

// thumbSize reads the {size} path value, refusing anything this archive does
// not render before it can become a path on disk.
func thumbSize(w http.ResponseWriter, r *http.Request) (int, bool) {
	size, err := strconv.Atoi(r.PathValue("size"))
	if err != nil || !derivstore.IsThumbSize(size) {
		writeError(w, http.StatusNotFound, "no rendition is stored at that size")
		return 0, false
	}
	return size, true
}

// handleLivePreview renders the 1080p rendition on demand and stores nothing.
//
// It is the photo preview's counterpart in every respect: same conditional
// check before any work is done, same immutable caching, same reasoning about
// why a file on disk would buy nothing. The one difference is that these bytes
// are held in memory for a while afterwards, because a video element asks for
// them more than once — see the livecache package.
func (s *Server) handleLivePreview(w http.ResponseWriter, r *http.Request) {
	video, ok := s.livePair(w, r)
	if !ok {
		return
	}
	if s.Video == nil || s.LivePreviews == nil {
		writeError(w, http.StatusNotFound, "this server cannot render live previews")
		return
	}

	etag := etagFor(video.SHA256, "live-preview")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", immutable)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	data, err := s.LivePreviews.Get(r.Context(), video.SHA256, func(ctx context.Context) ([]byte, error) {
		return s.renderLivePreview(ctx, video)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away mid-render
		}
		s.logger().Error("render live preview", "error", err, "sha256", video.SHA256)
		writeError(w, http.StatusInternalServerError, "could not render the live preview")
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	// ServeContent, so a player that opens with a range probe — which Safari
	// always does — gets a 206 rather than the whole file it did not ask for.
	http.ServeContent(w, r, video.SHA256+derivstore.LiveThumb, video.UploadedAt, bytes.NewReader(data))
}

// renderLivePreview transcodes one paired video and reads the result back.
//
// Through a staged file rather than a pipe, because `+faststart` rewrites the
// header once the encode is done and needs somewhere seekable to do it. Without
// it the whole clip would have to arrive before the first frame played, which
// for a press-and-hold is the entire interaction.
func (s *Server) renderLivePreview(ctx context.Context, video db.Asset) ([]byte, error) {
	staged, cleanup, err := s.Derivatives.Stage("livepreview-*" + derivstore.LiveThumb)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	source, release, err := s.assetPath(video)
	if err != nil {
		return nil, err
	}
	defer release()

	info, err := s.Video.Probe(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := s.Video.LivePreview(ctx, source, staged, info); err != nil {
		return nil, err
	}
	return os.ReadFile(staged)
}

// livePair resolves {id} to the Live Photo video it names.
//
// Either half works. The gallery holds stills and asks about those; anything
// holding the video's own id — a link, a second request from the viewer — is
// answered rather than told it asked the wrong question.
func (s *Server) livePair(w http.ResponseWriter, r *http.Request) (db.Asset, bool) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return db.Asset{}, false
	}
	if asset.IsLivePair() {
		return asset, true
	}

	video, err := s.Store.LiveVideoFor(r.Context(), asset.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "this asset is not a Live Photo")
		return db.Asset{}, false
	case err != nil:
		s.logger().Error("load paired video", "error", err, "asset", asset.ID)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return db.Asset{}, false
	}
	return video, true
}

func (s *Server) serveDerivative(w http.ResponseWriter, r *http.Request, asset db.Asset, suffix, contentType string) {
	if asset.Vault != "" {
		s.serveVaultDerivative(w, r, asset, suffix, contentType)
		return
	}

	f, err := s.Derivatives.Open(asset.SHA256, suffix)
	if err != nil {
		if os.IsNotExist(err) {
			// Not an error condition: the job may simply not have run yet.
			writeError(w, http.StatusNotFound, "derivative not generated yet")
			return
		}
		s.logger().Error("open derivative", "error", err, "sha256", asset.SHA256, "suffix", suffix)
		writeError(w, http.StatusInternalServerError, "could not read derivative")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.logger().Error("stat derivative", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "could not read derivative")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", immutable)
	w.Header().Set("ETag", etagFor(asset.SHA256, suffix))
	http.ServeContent(w, r, asset.SHA256+suffix, info.ModTime(), f)
}

// handlePreview renders the 2048px rendition on demand and stores nothing.
//
// For a Snapchat memory it renders the composite — the photograph with its
// caption layer over it, which is the picture that was actually sent and the
// only one anybody ever saw. The layer is a second archived asset, so this
// costs one more row read and one more decode, on the few hundred assets that
// have one.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	over, release, ok := s.previewLayer(w, r, asset)
	if !ok {
		return
	}
	defer release()
	s.servePreview(w, r, asset, over)
}

// handlePreviewPlain renders the same photograph without its caption layer.
//
// This is what the viewer's press-and-hold and its overlay toggle reach for. It
// answers for an asset that has no layer too, with the same bytes /preview
// would give: the URL means "the photograph itself", and that is a true answer
// for a photograph nobody drew on.
func (s *Server) handlePreviewPlain(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	s.servePreview(w, r, asset, nil)
}

// previewLayer resolves the caption layer to draw over an asset, if it has one.
func (s *Server) previewLayer(w http.ResponseWriter, r *http.Request, asset db.Asset) (*derive.Layer, func(), bool) {
	nothing := func() {}
	if asset.OverlayAssetID == nil {
		return nil, nothing, true
	}

	layer, err := s.Store.Asset(r.Context(), *asset.OverlayAssetID)
	if errors.Is(err, db.ErrNotFound) {
		// `on delete set null` means the database cannot get here, so this is
		// the archive disagreeing with itself. The photograph is worth more than
		// the caption, so it is drawn without one.
		s.logger().Warn("overlay is linked but missing", "asset", asset.ID)
		return nil, nothing, true
	}
	if err != nil {
		s.logger().Error("load overlay", "error", err, "asset", asset.ID)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return nil, nothing, false
	}

	// A vaulted layer is decrypted to a staged file, which has to outlive this
	// call because the render that reads it happens after it returns — so the
	// cleanup travels back to handlePreview rather than being deferred here.
	path, release, ok := s.sourcePath(w, layer)
	if !ok {
		return nil, nothing, false
	}

	return &derive.Layer{
		Path: path,
		// From the row rather than measured, because the metadata job has
		// already read them off the file. Null until it runs, which derive
		// treats as "measure it yourself".
		Width:  intOr(asset.Width),
		Height: intOr(asset.Height),
	}, release, true
}

// servePreview renders one 2048px rendition, with or without a layer over it.
//
// The conditional check happens before the conversion, not after: a client that
// already has these bytes must not cost an ImageMagick process to be told so.
// That is what makes arrow-keying back through a viewer cheap. The two
// renditions carry different ETags because they are different pictures, so a
// browser holding one is never handed it for the other.
func (s *Server) servePreview(w http.ResponseWriter, r *http.Request, asset db.Asset, over *derive.Layer) {
	if asset.MediaKind == db.MediaVideo {
		writeError(w, http.StatusNotFound, "videos have no preview; use /playback")
		return
	}

	variant := "preview"
	if over != nil {
		variant = "preview-composite"
	}
	etag := etagFor(asset.SHA256, variant)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", immutable)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	source, release, ok := s.sourcePath(w, asset)
	if !ok {
		return
	}
	defer release()

	// Buffered rather than streamed: a conversion that fails halfway must
	// produce an honest status code, not a truncated image with a 200 on it.
	var buf bytes.Buffer
	if err := s.Converter.Preview(r.Context(), source, over, &buf); err != nil {
		if r.Context().Err() != nil {
			return // client went away mid-conversion
		}
		s.logger().Error("render preview", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "could not render preview")
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	http.ServeContent(w, r, asset.SHA256+"."+variant+".webp", asset.UploadedAt, bytes.NewReader(buf.Bytes()))
}

func intOr(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func etagFor(sha256hex, variant string) string {
	return fmt.Sprintf(`"%s-%s"`, sha256hex, strings.TrimPrefix(variant, "."))
}

// etagMatches implements the If-None-Match comparison we need: a list of tags,
// or "*". Weak-comparison prefixes are ignored because nothing here issues them.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}
