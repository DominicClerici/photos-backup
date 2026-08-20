package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

const (
	defaultTimelineLimit = 200
	maxTimelineLimit     = 500
	// maxStateQuery bounds a poll to roughly what fits on screen at once.
	maxStateQuery = 500
)

// handleTimeline returns one page of the timeline, newest first.
//
// Two ways to say where the page starts, and never both. A cursor continues
// from the page before it, which is what a scroll down the grid does and what
// keeps sequential paging a keyset walk. An offset names a position in the day
// table, which is what a fling into the middle of the library does — see
// db.TimelineAt for why that one is allowed to be an OFFSET.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	limit := defaultTimelineLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxTimelineLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}

	var after *db.Cursor
	if raw := query.Get("cursor"); raw != "" {
		cursor, err := db.DecodeCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		after = &cursor
	}

	skip := 0
	if raw := query.Get("skip"); raw != "" {
		if after != nil {
			writeError(w, http.StatusBadRequest, "name at most one of cursor, skip")
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "skip must be a row offset")
			return
		}
		skip = n
	}

	filter, ok := timelineFilter(w, r)
	if !ok {
		return
	}

	var (
		page db.TimelinePage
		err  error
	)
	if skip > 0 {
		page, err = s.Store.TimelineAt(r.Context(), filter, skip, limit)
	} else {
		page, err = s.Store.Timeline(r.Context(), filter, after, limit)
	}
	if err != nil {
		s.writeFilterError(w, err, "read timeline")
		return
	}
	if page.Items == nil {
		page.Items = []db.TimelineItem{}
	}
	writeJSON(w, http.StatusOK, page)
}

// handleTimelineDays returns the shape of the whole filtered timeline: every
// heading it will draw and how many tiles hang under each.
//
// The gallery asks for this once, before the first page, and lays the entire
// grid out from it — so the scrollbar is the right height from the first frame
// and nothing moves as pages land. See db.DayTable.
//
// The timezone is a query parameter because the fallback for a file that
// recorded no UTC offset is the *viewer's* day, and the server has no way to
// know what that is. Absent or unrecognised, it is UTC.
func (s *Server) handleTimelineDays(w http.ResponseWriter, r *http.Request) {
	filter, ok := timelineFilter(w, r)
	if !ok {
		return
	}

	table, err := s.Store.TimelineDays(r.Context(), filter, r.URL.Query().Get("tz"))
	if err != nil {
		s.writeFilterError(w, err, "read timeline days")
		return
	}
	writeJSON(w, http.StatusOK, table)
}

// timelinePosition is where one asset sits in a timeline.
type timelinePosition struct {
	Index int `json:"index"`
}

// handleTimelineLocate answers where a linked asset is, so the gallery can go
// straight there instead of paging until it appears.
//
// It exists because the timeline is addressed by position and a shared link is
// not. Everything else the gallery needs — the geometry, the pages, the
// placeholders — is a position; this is the one translation, and without it a
// link to a five-year-old photograph costs a request per two hundred items
// ahead of it.
//
// The index is a position in the same day table the grid is drawn from, so the
// answer is something to scroll to and fetch, not just something to display.
func (s *Server) handleTimelineLocate(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "name an asset with id")
		return
	}

	filter, ok := timelineFilter(w, r)
	if !ok {
		return
	}

	index, err := s.Store.TimelinePosition(r.Context(), filter, id)
	if err != nil {
		// An id that is not a uuid arrives as a failed cast rather than as a
		// missing row, and both mean the same thing here — whichever of the two
		// ids was malformed, this timeline holds no position to go to. Answering
		// 400 for one and 404 for the other would be a distinction the caller
		// has no use for.
		if errors.Is(err, db.ErrNotFound) || isBadUUID(err) {
			writeError(w, http.StatusNotFound, "this timeline holds no such asset")
			return
		}
		s.writeFilterError(w, err, "locate asset in timeline")
		return
	}
	writeJSON(w, http.StatusOK, timelinePosition{Index: index})
}

// timelineFilter reads the collection a request narrows to, the facets it
// narrows by, and the order it wants them in.
//
// One collection at a time. Accepting two would be an intersection nothing asks
// for, and refusing it here keeps the query's shape a thing the gallery can
// reason about. The facets are the opposite and are meant to combine: they are
// adjectives rather than places. See db.TimelineFilter.
func timelineFilter(w http.ResponseWriter, r *http.Request) (db.TimelineFilter, bool) {
	query := r.URL.Query()

	sort, err := db.ParseSort(query.Get("sort"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "sort must be newest, oldest, longest or shortest")
		return db.TimelineFilter{}, false
	}
	kind, ok := mediaKind(w, query.Get("kind"))
	if !ok {
		return db.TimelineFilter{}, false
	}

	filter := db.TimelineFilter{
		AlbumID:  query.Get("album"),
		Person:   query.Get("person"),
		Category: query.Get("category"),
		// Not counted below, because it is not a collection. It replaces the
		// rule that says what the timeline is over rather than narrowing it, so
		// asking for the trash *and* an album is a coherent question — "what of
		// this album have I deleted" — even though nothing asks it yet.
		Trash: truthy(query.Get("trash")),

		Sort:      sort,
		Kind:      kind,
		Favorites: truthy(query.Get("favorites")),
		Unalbumed: truthy(query.Get("unalbumed")),
	}
	if named(filter) > 1 {
		writeError(w, http.StatusBadRequest, "name at most one of album, person, category")
		return db.TimelineFilter{}, false
	}
	return filter, true
}

// mediaKind reads the media-kind facet, which is empty for both kinds.
//
// Refused rather than ignored when it is neither, for the reason an unknown
// category is: a filter that quietly widens to the whole library is a bug that
// looks like it works.
func mediaKind(w http.ResponseWriter, kind string) (string, bool) {
	switch kind {
	case "", db.MediaImage, db.MediaVideo:
		return kind, true
	}
	writeError(w, http.StatusBadRequest, "kind must be image or video")
	return "", false
}

// writeFilterError answers for the two queries that share a filter. Both can
// fail on what the request named rather than on the archive, and both should
// say so with a 400 instead of blaming the database.
func (s *Server) writeFilterError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, db.ErrUnknownCategory):
		writeError(w, http.StatusBadRequest, "unknown category")
	case errors.Is(err, db.ErrUnknownKind):
		writeError(w, http.StatusBadRequest, "kind must be image or video")
	case isBadUUID(err):
		writeError(w, http.StatusBadRequest, "malformed album id")
	default:
		s.logger().Error(what, "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
	}
}

// named counts how many collections a filter picks out.
func named(f db.TimelineFilter) int {
	n := 0
	for _, v := range []string{f.AlbumID, f.Person, f.Category} {
		if v != "" {
			n++
		}
	}
	return n
}

// handleTimelineStates re-reports the state of specific assets, which is how
// the gallery watches its pending tiles fill in during a backfill without
// re-fetching whole pages.
//
// POST rather than GET because the request carries up to 500 ids, which is well
// past what belongs in a query string.
func (s *Server) handleTimelineStates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be {\"ids\": [...]}")
		return
	}
	if len(body.IDs) > maxStateQuery {
		writeError(w, http.StatusBadRequest, "too many ids")
		return
	}

	states, err := s.Store.TimelineStates(r.Context(), body.IDs)
	if err != nil {
		if isBadUUID(err) {
			writeError(w, http.StatusBadRequest, "malformed asset id")
			return
		}
		s.logger().Error("read asset states", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	items := make([]db.TimelineItem, 0, len(states))
	for _, it := range states {
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// assetDetail is what the viewer's metadata panel shows. It is a separate shape
// from TimelineItem on purpose: the timeline carries thousands of items and
// stays tiny, this one is fetched for a single open asset and can be generous.
type assetDetail struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaKind string `json:"kind"`
	SHA256    string `json:"sha256"`
	ByteSize  int64  `json:"byte_size"`

	Width           *int     `json:"width,omitempty"`
	Height          *int     `json:"height,omitempty"`
	DurationSeconds *float64 `json:"duration,omitempty"`

	// TakenAt is the timeline's sort key. OffsetMinutes is the file's own UTC
	// offset when it recorded one; nil means the zone is unknown and the viewer
	// should not claim to know what the local time was.
	TakenAt       time.Time `json:"taken_at"`
	OffsetMinutes *int      `json:"offset_minutes,omitempty"`
	// ReportedAt is the phone's capture time, kept beside the file's so a
	// disagreement is visible rather than hidden.
	ReportedAt *time.Time `json:"reported_at,omitempty"`
	UploadedAt time.Time  `json:"uploaded_at"`

	CameraMake  string `json:"camera_make,omitempty"`
	CameraModel string `json:"camera_model,omitempty"`
	Lens        string `json:"lens,omitempty"`

	GPSLat *float64 `json:"gps_lat,omitempty"`
	GPSLon *float64 `json:"gps_lon,omitempty"`

	// What an import knew and the file did not. Absent on anything a device
	// uploaded directly, which is why every one of these is omitempty.
	Description string   `json:"description,omitempty"`
	Favorite    bool     `json:"favorite,omitempty"`
	Archived    bool     `json:"archived,omitempty"`
	Albums      []string `json:"albums,omitempty"`
	People      []string `json:"people,omitempty"`

	// HasOverlay says the renditions above are composites, and that the plain
	// routes will answer for this asset. It is the whole of what the viewer
	// needs to offer the toggle, which is why the overlay's own id is not here:
	// the layer is never addressed directly by anything.
	HasOverlay bool `json:"has_overlay,omitempty"`

	State         string `json:"state"`
	PlaybackState string `json:"playback_state,omitempty"`
}

func (s *Server) handleAssetDetail(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
	}

	// A hidden photograph opens into the same viewer with the same panel, so
	// the panel has to be filled from the sealed document rather than from the
	// row the scrub emptied. It is the one handler where the two halves of the
	// archive answer from different places, and it is worth it: a panel that
	// went blank on hidden photographs would be a second viewer by omission.
	if asset.Vault != "" {
		s.vaultDetail(w, r, asset)
		return
	}

	// Two small queries rather than a join onto the asset load, because these
	// tables are empty for most archives and the asset load is on every
	// timeline page's critical path while this handler is not.
	extras, err := s.Store.AssetExtras(r.Context(), asset.ID)
	if err != nil {
		// Not fatal. A viewer that cannot list albums is worth more than one
		// that refuses to open the photo.
		s.logger().Warn("load asset extras", "error", err, "asset", asset.ID)
	}

	writeJSON(w, http.StatusOK, assetDetail{
		ID:              asset.ID,
		Filename:        asset.OriginalFilename,
		MediaKind:       asset.MediaKind,
		SHA256:          asset.SHA256,
		ByteSize:        asset.ByteSize,
		Width:           asset.Width,
		Height:          asset.Height,
		DurationSeconds: asset.DurationSeconds,
		TakenAt:         asset.SortTime,
		OffsetMinutes:   asset.ExifOffsetMinutes,
		ReportedAt:      asset.CapturedAt,
		UploadedAt:      asset.UploadedAt,
		CameraMake:      asset.CameraMake,
		CameraModel:     asset.CameraModel,
		Lens:            asset.Lens,
		GPSLat:          asset.GPSLat,
		GPSLon:          asset.GPSLon,
		Description:     asset.Description,
		Favorite:        asset.Favorite,
		Archived:        asset.Archived,
		Albums:          extras.Albums,
		People:          extras.People,
		HasOverlay:      asset.OverlayAssetID != nil,
		State:           asset.DerivedState,
		PlaybackState:   asset.PlaybackState,
	})
}

type jobsSummary struct {
	Counts []jobs.Count     `json:"counts"`
	Failed []jobs.FailedJob `json:"failed"`
}

// handleJobs is the "what is the worker doing, and what is broken" view. Queue
// depth answers the first question; the failure list answers the second.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.Queue == nil {
		writeError(w, http.StatusNotFound, "no worker queue on this server")
		return
	}

	counts, err := s.Queue.Counts(r.Context())
	if err != nil {
		s.logger().Error("count jobs", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	failed, err := s.Queue.Failed(r.Context(), 50)
	if err != nil {
		s.logger().Error("list failed jobs", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	if counts == nil {
		counts = []jobs.Count{}
	}
	if failed == nil {
		failed = []jobs.FailedJob{}
	}
	writeJSON(w, http.StatusOK, jobsSummary{Counts: counts, Failed: failed})
}

type health struct {
	OK bool `json:"ok"`
	// FailedJobs is surfaced here so a permanently broken derivative is
	// something you can find out about without opening the gallery.
	FailedJobs  int64 `json:"failed_jobs"`
	PendingJobs int64 `json:"pending_jobs"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := health{OK: true}

	if s.Queue != nil {
		counts, err := s.Queue.Counts(r.Context())
		if err != nil {
			// The queue being unreadable does not make the server unhealthy for
			// the purpose that matters: uploads still commit.
			s.logger().Warn("count jobs for health", "error", err)
		}
		for _, c := range counts {
			switch c.State {
			case jobs.StateFailed:
				h.FailedJobs += c.Count
			case jobs.StatePending, jobs.StateRunning:
				h.PendingJobs += c.Count
			}
		}
	}
	writeJSON(w, http.StatusOK, h)
}
