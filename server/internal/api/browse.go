package api

import (
	"encoding/json"
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
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	limit := defaultTimelineLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxTimelineLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}

	var after *db.Cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err := db.DecodeCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		after = &cursor
	}

	page, err := s.Store.Timeline(r.Context(), after, limit)
	if err != nil {
		s.logger().Error("read timeline", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if page.Items == nil {
		page.Items = []db.TimelineItem{}
	}
	writeJSON(w, http.StatusOK, page)
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

	State         string `json:"state"`
	PlaybackState string `json:"playback_state,omitempty"`
}

func (s *Server) handleAssetDetail(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookup(w, r)
	if !ok {
		return
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
