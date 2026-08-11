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

// handlePlayback serves the browser-playable rendition of a video.
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

	info, err := s.Video.Probe(ctx, s.Blobs.Path(video.SHA256, video.Ext))
	if err != nil {
		return nil, err
	}
	if err := s.Video.LivePreview(ctx, s.Blobs.Path(video.SHA256, video.Ext), staged, info); err != nil {
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
// The conditional check happens before the conversion, not after: a client that
// already has these bytes must not cost an ImageMagick process to be told so.
// That is what makes arrow-keying back through a viewer cheap.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if asset.MediaKind == db.MediaVideo {
		writeError(w, http.StatusNotFound, "videos have no preview; use /playback")
		return
	}

	etag := etagFor(asset.SHA256, "preview")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", immutable)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Buffered rather than streamed: a conversion that fails halfway must
	// produce an honest status code, not a truncated image with a 200 on it.
	var buf bytes.Buffer
	if err := s.Converter.Preview(r.Context(), s.Blobs.Path(asset.SHA256, asset.Ext), &buf); err != nil {
		if r.Context().Err() != nil {
			return // client went away mid-conversion
		}
		s.logger().Error("render preview", "error", err, "sha256", asset.SHA256)
		writeError(w, http.StatusInternalServerError, "could not render preview")
		return
	}

	w.Header().Set("Content-Type", "image/webp")
	http.ServeContent(w, r, asset.SHA256+".preview.webp", asset.UploadedAt, bytes.NewReader(buf.Bytes()))
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
