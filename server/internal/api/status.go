package api

import (
	"context"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/diskusage"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// statusResponse is the whole of the status page in one request.
//
// One endpoint rather than five, because every card on that page is a claim
// about the same instant. Fetched separately, a reload could show a queue that
// had drained against a failure list from before it did, and the page would be
// describing a server that never existed.
type statusResponse struct {
	Library  db.LibraryStats `json:"library"`
	Storage  storageStatus   `json:"storage"`
	Queue    queueStatus     `json:"queue"`
	Problems []problem       `json:"problems"`
	Failures []failure       `json:"failures"`
}

// storageStatus is the drive, and what of it this archive accounts for.
type storageStatus struct {
	// Archive is the volume the originals are on: the one the pie is drawn of,
	// and the one that runs out.
	Archive diskusage.Volume `json:"archive"`
	// Derivatives is the volume the renditions are on. Normally a different
	// disk — the deployment puts the blobs on the external drive and the
	// thumbnails on the SSD — which is why it is a volume of its own here
	// rather than a slice of the one above.
	Derivatives diskusage.Volume `json:"derivatives"`
	// SameVolume says whether the two are one filesystem, which is the
	// development default. When they are, the derivative bytes belong inside
	// the archive pie; when they are not, adding them in would draw space that
	// is not on that disk.
	SameVolume bool `json:"same_volume"`

	Photos           int64 `json:"photos"`
	Videos           int64 `json:"videos"`
	PhotoDerivatives int64 `json:"photo_derivatives"`
	VideoDerivatives int64 `json:"video_derivatives"`
	// Unattributed is every rendition whose original is gone or in the vault.
	// Small, and named rather than folded into the remainder so that a tree
	// full of orphans is something the page can eventually say out loud.
	Unattributed int64 `json:"unattributed_derivatives"`

	// MeasuredAt is when the walk behind these figures ran. The counts above it
	// are live; the derivative sizes are minutes old at worst, which is the
	// price of not walking a hundred thousand files on every poll.
	MeasuredAt time.Time `json:"measured_at"`
}

// queueStatus is the worker's backlog, and what it is chewing on.
type queueStatus struct {
	Pending int64 `json:"pending"`
	Running int64 `json:"running"`
	Failed  int64 `json:"failed"`
	// Kinds is the same total split by job kind, so "3,412 queued" can say
	// whether that is thumbnails the gallery is waiting on or a signature
	// backfill nobody is.
	Kinds []jobs.Count `json:"kinds"`
}

// A problem is something wrong with the server rather than with one photograph.
//
// Severity is "error" when the archive is currently losing work — an upload
// that will queue a job nothing drains — and "warning" when it is degraded but
// coping.
type problem struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// A failure is one job that gave up, with enough about its asset to recognise
// it — and enough of the error to hand to somebody who can fix it.
type failure struct {
	ID       int64     `json:"id"`
	Kind     jobs.Kind `json:"kind"`
	AssetID  string    `json:"asset_id"`
	Attempts int       `json:"attempts"`
	Error    string    `json:"error"`
	FailedAt time.Time `json:"failed_at"`

	Filename  string `json:"filename,omitempty"`
	MediaKind string `json:"media_kind,omitempty"`
	// Viewable is false when there is no thumbnail to draw: the asset is in the
	// vault, or it is not there at all any more.
	Viewable bool `json:"viewable"`
}

// Tool is an external binary the archive degrades without. Set from the
// daemon's config so the status page checks the same paths the workers use,
// rather than whatever happens to be on the API process's PATH under a
// different name.
type Tool struct {
	Binary string
	Needs  string
}

// failureLimit is how many failed jobs the page is sent. A queue with more than
// this many failures has one cause, not fifty — the list is for recognising the
// pattern, and the whole table is still at /v1/jobs.
const failureLimit = 100

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	library, err := s.Store.LibraryStats(ctx)
	if err != nil {
		s.logger().Error("count library for status", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	out := statusResponse{
		Library:  library,
		Storage:  s.storage(ctx),
		Problems: []problem{},
		Failures: []failure{},
	}

	// Everything below is degraded rather than fatal, the way handleHealth is:
	// a page that refuses to draw the count it has because it could not read
	// the queue tells you less than one that draws it and says so.
	if s.Queue == nil {
		out.Problems = append(out.Problems, problem{
			ID:       "queue-missing",
			Severity: "error",
			Title:    "No job queue",
			Detail:   "This server is running without a queue, so nothing will ever build a thumbnail or a playback rendition.",
		})
	} else {
		counts, err := s.Queue.Counts(ctx)
		if err != nil {
			s.logger().Warn("count jobs for status", "error", err)
			out.Problems = append(out.Problems, problem{
				ID:       "queue-unreadable",
				Severity: "warning",
				Title:    "Queue could not be read",
				Detail:   err.Error(),
			})
		}
		out.Queue.Kinds = counts
		for _, c := range counts {
			switch c.State {
			case jobs.StatePending:
				out.Queue.Pending += c.Count
			case jobs.StateRunning:
				out.Queue.Running += c.Count
			case jobs.StateFailed:
				out.Queue.Failed += c.Count
			}
		}

		failed, err := s.Queue.Failed(ctx, failureLimit)
		if err != nil {
			s.logger().Warn("list failed jobs for status", "error", err)
		}
		out.Failures = s.describe(ctx, failed)
	}
	if out.Queue.Kinds == nil {
		out.Queue.Kinds = []jobs.Count{}
	}

	out.Problems = append(out.Problems, s.degradations()...)
	writeJSON(w, http.StatusOK, out)
}

// describe attaches each failure's asset to it, in one query rather than one
// per row.
func (s *Server) describe(ctx context.Context, failed []jobs.FailedJob) []failure {
	out := make([]failure, 0, len(failed))
	ids := make([]string, 0, len(failed))
	for _, f := range failed {
		ids = append(ids, f.AssetID)
	}

	labels, err := s.Store.AssetLabels(ctx, ids)
	if err != nil {
		// The error text is the point of this list, and it is already in hand.
		// A missing filename makes the row harder to recognise; it does not
		// make it worth withholding.
		s.logger().Warn("label failed assets", "error", err)
		labels = map[string]db.AssetLabel{}
	}

	for _, f := range failed {
		row := failure{
			ID:       f.ID,
			Kind:     f.Kind,
			AssetID:  f.AssetID,
			Attempts: f.Attempts,
			Error:    f.Error,
			FailedAt: f.FailedAt,
		}
		if label, ok := labels[f.AssetID]; ok {
			row.Filename = label.Filename
			row.MediaKind = label.MediaKind
			row.Viewable = label.Viewable
		}
		out = append(out, row)
	}
	return out
}

// degradations are the problems that are true of the server itself rather than
// of anything in the database.
func (s *Server) degradations() []problem {
	var out []problem

	if !s.WorkerEnabled {
		out = append(out, problem{
			ID:       "worker-disabled",
			Severity: "error",
			Title:    "Derivative workers are off",
			Detail:   "WORKER_DISABLED is set on this server. Uploads still reach the disk, but nothing drains the queue: new items will have no thumbnail and no playable rendition until the workers are started.",
		})
	}

	for _, tool := range s.Tools {
		if _, err := exec.LookPath(tool.Binary); err != nil {
			out = append(out, problem{
				ID:       "tool-" + tool.Binary,
				Severity: "warning",
				Title:    tool.Binary + " not found",
				Detail:   "Uploads still work, but " + tool.Needs + " will fail until it is installed or its path is set.",
			})
		}
	}

	if s.Vault == nil {
		out = append(out, problem{
			ID:       "vault-unavailable",
			Severity: "warning",
			Title:    "Vault unavailable",
			Detail:   "The Archive and Hidden buckets are not mounted on this server. Nothing has been lost; they cannot be opened from here.",
		})
	}

	if s.Scan == nil {
		out = append(out, problem{
			ID:       "scan-unavailable",
			Severity: "warning",
			Title:    "Duplicate scan unavailable",
			Detail:   "This server cannot look for duplicates or split recordings, so those counts are only as fresh as the last server that did.",
		})
	}

	return out
}

// storageCache holds the derivative walk between requests.
//
// The status page polls, and the walk is a stat of every file under the
// derivatives root. Once a minute is often enough for a figure that moves by
// megabytes an hour, and the volume figures beside it are re-read every time
// because they are two syscalls.
type storageCache struct {
	mu    sync.Mutex
	at    time.Time
	photo int64
	video int64
	other int64
}

// derivativeTTL is how stale the walk is allowed to get. Short enough that
// clearing the tree shows up while you are still looking at the page.
const derivativeTTL = time.Minute

func (s *Server) storage(ctx context.Context) storageStatus {
	out := storageStatus{}

	if s.Blobs != nil {
		if v, err := diskusage.Stat(s.Blobs.Root()); err != nil {
			s.logger().Warn("stat archive volume", "error", err)
		} else {
			out.Archive = v
		}
	}
	if s.Derivatives != nil {
		if v, err := diskusage.Stat(s.Derivatives.Root()); err != nil {
			s.logger().Warn("stat derivatives volume", "error", err)
		} else {
			out.Derivatives = v
		}
	}
	out.SameVolume = diskusage.SameVolume(out.Archive, out.Derivatives)

	if bytes, err := s.Store.StoredBytes(ctx); err != nil {
		s.logger().Warn("sum stored bytes", "error", err)
	} else {
		out.Photos, out.Videos = bytes.Photos, bytes.Videos
	}

	photo, video, other, at := s.derivativeBytes(ctx)
	out.PhotoDerivatives, out.VideoDerivatives, out.Unattributed = photo, video, other
	out.MeasuredAt = at
	return out
}

// derivativeBytes walks the derivative tree, or reuses the last walk.
func (s *Server) derivativeBytes(ctx context.Context) (photo, video, other int64, at time.Time) {
	if s.Derivatives == nil {
		return 0, 0, 0, time.Time{}
	}

	s.deriv.mu.Lock()
	defer s.deriv.mu.Unlock()

	if !s.deriv.at.IsZero() && time.Since(s.deriv.at) < derivativeTTL {
		return s.deriv.photo, s.deriv.video, s.deriv.other, s.deriv.at
	}

	kinds, err := s.Store.MediaKinds(ctx)
	if err != nil {
		s.logger().Warn("load media kinds for storage", "error", err)
		return s.deriv.photo, s.deriv.video, s.deriv.other, s.deriv.at
	}

	// A rejected join is named after a merge group rather than an asset, so
	// MediaKinds has never heard of it and it would land in the unattributed
	// remainder — a figure that is supposed to mean "hidden or purged", not
	// "a feature's working set". It is video by construction.
	if previews, err := s.Store.SegmentPreviews(ctx); err != nil {
		s.logger().Warn("load rejected joins for storage", "error", err)
	} else {
		for _, fingerprint := range previews {
			kinds[fingerprint] = db.MediaVideo
		}
	}

	usage, err := s.Derivatives.Usage(func(sha string) string { return kinds[sha] })
	if err != nil {
		s.logger().Warn("measure derivatives", "error", err)
		return s.deriv.photo, s.deriv.video, s.deriv.other, s.deriv.at
	}

	s.deriv.photo = usage["image"]
	s.deriv.video = usage["video"]
	s.deriv.other = usage[""]
	s.deriv.at = time.Now()
	return s.deriv.photo, s.deriv.video, s.deriv.other, s.deriv.at
}
