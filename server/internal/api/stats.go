package api

import (
	"net/http"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// statsResponse is the app's backup card: what this phone has archived, and
// what the archive holds altogether.
type statsResponse struct {
	Device  db.DeviceStats `json:"device"`
	Archive archiveStats   `json:"archive"`
}

// archiveStats is db.ArchiveStats plus the derivative queue, which lives in a
// different table and is not the store's to count.
type archiveStats struct {
	db.ArchiveStats
	// PendingJobs and FailedJobs are why the archive block is worth sending at
	// all: an asset can be safely stored and still have no thumbnail, and
	// without these the app would show a backup that looks finished while the
	// worker is hours behind.
	PendingJobs int64 `json:"pending_jobs"`
	FailedJobs  int64 `json:"failed_jobs"`
}

// handleStats reports the archive from one device's point of view.
//
// The only read whose answer depends on who is asking, which is why it is not
// in readRoutes — see the note at its registration in Handler.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, device devices.Device) {
	deviceStats, err := s.Store.DeviceStats(r.Context(), device.ID)
	if err != nil {
		s.logger().Error("count device assets", "error", err, "device_id", device.ID)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	archive, err := s.Store.ArchiveStats(r.Context())
	if err != nil {
		s.logger().Error("count archive assets", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	out := statsResponse{Device: deviceStats, Archive: archiveStats{ArchiveStats: archive}}

	// Degraded rather than fatal, as in handleHealth: an unreadable queue does
	// not make the counts that matter wrong, and refusing the whole response
	// over it would blank the card for a reason nobody reading it cares about.
	if s.Queue != nil {
		counts, err := s.Queue.Counts(r.Context())
		if err != nil {
			s.logger().Warn("count jobs for stats", "error", err)
		}
		for _, c := range counts {
			switch c.State {
			case jobs.StateFailed:
				out.Archive.FailedJobs += c.Count
			case jobs.StatePending, jobs.StateRunning:
				out.Archive.PendingJobs += c.Count
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}
