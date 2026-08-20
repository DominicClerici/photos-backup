package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

// maxGroups bounds one page of the review. A library that has never been tidied
// can have hundreds of pending groups, and the page draws every member of every
// one of them as a thumbnail — so this is a limit on how many images a browser
// is asked to hold, not on how many questions there are.
const maxGroups = 60

// handleMergeCounts is the overview card: how much is waiting, and how much of
// the library the answer is based on.
func (s *Server) handleMergeCounts(w http.ResponseWriter, r *http.Request) {
	counts, err := s.Store.MergeCounts(r.Context())
	if err != nil {
		s.logger().Error("count merge groups", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

// handleMergeGroups lists groups of one kind and state.
//
// Both are query parameters against closed lists rather than paths, because
// they are two axes of one collection and every combination of them is
// meaningful: the pending duplicates are the review, the merged segments are
// what the worker did overnight, and the dismissed anything is how somebody
// checks what they said no to.
func (s *Server) handleMergeGroups(w http.ResponseWriter, r *http.Request) {
	query := db.MergeQuery{Limit: maxGroups, Approved: boolParam(r, "approved")}

	query.Kind = r.URL.Query().Get("kind")
	switch query.Kind {
	case merge.KindDuplicate, merge.KindSegments:
	case "":
		query.Kind = merge.KindDuplicate
	default:
		writeError(w, http.StatusBadRequest, "kind must be "+merge.KindDuplicate+" or "+merge.KindSegments)
		return
	}

	query.State = r.URL.Query().Get("state")
	switch query.State {
	case db.MergePending, db.MergeMerged, db.MergeDismissed, db.MergeFailedState:
	case "":
		query.State = db.MergePending
	default:
		writeError(w, http.StatusBadRequest, "state must be pending, merged, dismissed or failed")
		return
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		query.Limit = min(n, maxGroups)
	}

	groups, err := s.Store.Groups(r.Context(), query)
	if err != nil {
		s.logger().Error("read merge groups", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	// An explicit empty list rather than a null, so the client has one shape to
	// render and "none" is not a special case in the browser.
	out := make([]mergeGroupView, 0, len(groups))
	for _, g := range groups {
		out = append(out, mergeGroupView{MergeGroup: g, Preview: s.hasJoinPreview(g)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// mergeGroupView is a group as the review page needs it: the row, plus the one
// thing about it that is a fact about the disk rather than the database.
type mergeGroupView struct {
	db.MergeGroup
	// Preview is true when a join this group's last attempt refused to archive
	// is still there to be watched. It decides whether the page offers the
	// button, so that "watch it" is never a link to a 404 — and it is false for
	// every failure that never produced a file, which is most kinds.
	Preview bool `json:"preview,omitempty"`
}

// hasJoinPreview reports whether a rejected join is on disk for this group.
func (s *Server) hasJoinPreview(g db.MergeGroup) bool {
	return s.Derivatives != nil && g.Kind == merge.KindSegments &&
		s.Derivatives.Exists(g.Fingerprint, derivstore.JoinPreview)
}

// boolParam reads a query flag written the way a browser writes one.
func boolParam(r *http.Request, name string) bool {
	switch r.URL.Query().Get(name) {
	case "1", "true", "yes":
		return true
	}
	return false
}

type mergeRequest struct {
	// Keeper is the copy to keep, and it must be a member of the group.
	//
	// Required rather than defaulted to the group's first member. The order the
	// members come back in is merge.Rank's opinion about which is the better
	// copy, the page preselects it, and somebody can change it — so a request
	// that named nothing would be ambiguous between "I agree with the
	// suggestion" and "I forgot to send the field", and only one of those
	// should trash three photographs.
	Keeper string `json:"keeper"`
}

// handleMerge resolves one group by hand: keep this copy, trash the rest.
//
// Only ever a duplicate group. A set of video segments is resolved by the
// worker that builds the joined recording, because there is no copy among them
// to keep — the thing that survives is a file that does not exist until the
// merge runs. Asking this endpoint to do one would mean electing one ten-second
// piece and throwing away the rest of the minute.
func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body: "+err.Error())
		return
	}
	if req.Keeper == "" {
		writeError(w, http.StatusBadRequest, "keeper is required: say which copy to keep")
		return
	}

	id := r.PathValue("id")
	group, ok := s.lookupGroup(w, r, id)
	if !ok {
		return
	}
	if group.Kind != merge.KindDuplicate {
		writeError(w, http.StatusConflict,
			"a set of video segments is joined by the worker, not resolved by choosing one of them")
		return
	}

	result, err := s.Store.MergeDuplicate(r.Context(), id, req.Keeper)
	switch {
	case errors.Is(err, db.ErrNotPending):
		writeError(w, http.StatusConflict, "that group has already been resolved")
		return
	case errors.Is(err, db.ErrNotAMember):
		writeError(w, http.StatusBadRequest, "that photo is not one of this group's copies")
		return
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such group")
		return
	case err != nil:
		s.logger().Error("merge duplicates", "error", err, "group", id)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDismissMerge records that somebody looked and said no.
//
// For duplicates that is "these are different photographs". For a set of
// segments whose join keeps failing it is "leave them as separate clips", which
// is the only way out of that state that does not involve overruling the
// duration check — and it takes the failed job off the queue with it, so the
// status page stops reporting work nobody wants done.
func (s *Server) handleDismissMerge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	group, ok := s.lookupGroup(w, r, id)
	if !ok {
		return
	}

	err := s.Store.DismissGroup(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotPending):
		writeError(w, http.StatusConflict, "that group has already been resolved")
		return
	case err != nil:
		s.logger().Error("dismiss merge group", "error", err, "group", id)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	s.dropJoinPreview(group)
	w.WriteHeader(http.StatusNoContent)
}

// dropJoinPreview removes a group's rejected join, once the group has stopped
// being a question that file is evidence about.
//
// Best effort, and not the only thing that does it: the sweep in photod
// reconciles the whole tree against db.SegmentPreviews, so a failure here costs
// a few megabytes until the next one rather than leaking a file forever.
func (s *Server) dropJoinPreview(group db.MergeGroup) {
	if s.Derivatives == nil || group.Kind != merge.KindSegments {
		return
	}
	if err := s.Derivatives.Remove(group.Fingerprint, derivstore.JoinPreview); err != nil {
		s.logger().Warn("could not remove a rejected join", "error", err, "group", group.ID)
	}
}

// handleUnmerge puts a merge back: the copies come out of the trash, and a
// joined recording goes into it.
//
// The undo for the half of this feature that happens without being asked. A
// recording joined from six pieces is the one thing here that changes the
// library on its own, so the page that lists what was joined has a button
// beside every row — see db.UnmergeGroup for why it lands in `dismissed` rather
// than back in `pending`.
func (s *Server) handleUnmerge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	group, ok := s.lookupGroup(w, r, id)
	if !ok {
		return
	}

	out, err := s.Store.UnmergeGroup(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such merged group")
		return
	case err != nil:
		s.logger().Error("undo a merge", "error", err, "group", id)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	s.dropJoinPreview(group)
	writeJSON(w, http.StatusOK, out)
}

// handleScan re-runs the detection over the whole library.
//
// A write, though it creates no photograph and destroys none: it proposes
// questions, and it queues the joins that answer one kind of them without being
// asked. It is on the review page rather than on a timer because the things
// that create work here — an import, a signature backfill — both end, and a
// sweep that ran hourly over an untouched library would write nothing.
//
// Synchronous. It is a few seconds over this archive, the page that called it
// is about to redraw from what it found, and a version that returned
// immediately would leave the browser polling for a result it has no handle on.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.Scan == nil {
		// No worker in this process. The tables are still readable and the
		// review page still works on whatever is already in them; what is not
		// available is finding more.
		writeError(w, http.StatusServiceUnavailable,
			"this server has no worker, so it cannot scan; the results of an earlier scan are still readable")
		return
	}
	result, err := s.Scan(r.Context())
	if err != nil {
		s.logger().Error("scan for duplicates and split recordings", "error", err)
		writeError(w, http.StatusInternalServerError, "the scan failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleApproveMerge records that somebody has read one entry of the joined
// recordings log and is content with it, or takes that back.
//
// It changes nothing about the photographs, which is why it is one click with
// no confirmation and why the undo is the same click again. What it changes is
// what the page and the status card ask for attention about: the log only ever
// grows, every row on it has already happened, and without this there is no way
// to say a row has been read.
func (s *Server) handleApproveMerge(w http.ResponseWriter, r *http.Request) {
	s.setApproved(w, r, true)
}

func (s *Server) handleUnapproveMerge(w http.ResponseWriter, r *http.Request) {
	s.setApproved(w, r, false)
}

func (s *Server) setApproved(w http.ResponseWriter, r *http.Request, approved bool) {
	id := r.PathValue("id")
	if _, ok := s.lookupGroup(w, r, id); !ok {
		return
	}

	err := s.Store.ApproveGroup(r.Context(), id, approved)
	switch {
	case errors.Is(err, db.ErrNotMerged):
		writeError(w, http.StatusConflict, "only a merge that has happened can be approved")
		return
	case err != nil:
		s.logger().Error("approve merge group", "error", err, "group", id)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleForceJoin archives a join this server refused to make.
//
// The refusal is video.joinDurationSlack: the concatenated file did not come
// out the length its parts add up to, and the arithmetic cannot tell a part
// ffmpeg dropped from a container whose last frame runs long. A person watching
// the rejected file can, which is why this endpoint's precondition is not
// technical — it is that /preview exists and has been looked at.
//
// It queues rather than joins. The work is a minute of ffmpeg in the pool that
// is already doing exactly this, and the flag it sets outlives every retry that
// pool makes.
func (s *Server) handleForceJoin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	group, ok := s.lookupGroup(w, r, id)
	if !ok {
		return
	}
	if group.Kind != merge.KindSegments {
		writeError(w, http.StatusConflict, "only a set of video segments is joined")
		return
	}

	err := s.Store.ForceJoin(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotPending):
		writeError(w, http.StatusConflict, "that group has already been resolved")
		return
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such group")
		return
	case err != nil:
		s.logger().Error("force a join", "error", err, "group", id)
		writeError(w, http.StatusServiceUnavailable, "could not queue the join: "+err.Error())
		return
	}
	if s.Nudge != nil {
		s.Nudge()
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

// handleJoinPreview serves the join a merge job built and then refused to
// archive.
//
// The one route in this server that serves a video belonging to no asset. It
// has no asset because that is exactly what failed — the file was never
// committed, never indexed, and is not in the library — and it is served anyway
// because the decision it is evidence for cannot be made any other way.
//
// Not immutable, unlike every other derivative: a requeued join overwrites it,
// and a browser holding the previous attempt for a year would be showing
// somebody the wrong minute of video to decide about.
func (s *Server) handleJoinPreview(w http.ResponseWriter, r *http.Request) {
	group, ok := s.lookupGroup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if s.Derivatives == nil || group.Kind != merge.KindSegments {
		writeError(w, http.StatusNotFound, "no join to preview")
		return
	}

	f, err := s.Derivatives.Open(group.Fingerprint, derivstore.JoinPreview)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound,
				"there is no rejected join for this group; it may have been joined, or it may have failed before ffmpeg produced anything")
			return
		}
		s.logger().Error("open a rejected join", "error", err, "group", group.ID)
		writeError(w, http.StatusInternalServerError, "could not read the rejected join")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.logger().Error("stat a rejected join", "error", err, "group", group.ID)
		writeError(w, http.StatusInternalServerError, "could not read the rejected join")
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, group.Fingerprint+derivstore.JoinPreview, info.ModTime(), f)
}

// lookupGroup resolves the {id} path value, answering with the right status
// when it is unknown — the same shape as Server.lookup does for an asset.
func (s *Server) lookupGroup(w http.ResponseWriter, r *http.Request, id string) (db.MergeGroup, bool) {
	group, err := s.Store.Group(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such group")
		return db.MergeGroup{}, false
	case err != nil:
		if isBadUUID(err) {
			writeError(w, http.StatusNotFound, "no such group")
			return db.MergeGroup{}, false
		}
		s.logger().Error("read merge group", "error", err, "group", id)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return db.MergeGroup{}, false
	}
	return group, true
}
