package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/dominicclerici/photos-backup/server/internal/db"
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
	kind := r.URL.Query().Get("kind")
	switch kind {
	case merge.KindDuplicate, merge.KindSegments:
	case "":
		kind = merge.KindDuplicate
	default:
		writeError(w, http.StatusBadRequest, "kind must be "+merge.KindDuplicate+" or "+merge.KindSegments)
		return
	}

	state := r.URL.Query().Get("state")
	switch state {
	case db.MergePending, db.MergeMerged, db.MergeDismissed:
	case "":
		state = db.MergePending
	default:
		writeError(w, http.StatusBadRequest, "state must be pending, merged or dismissed")
		return
	}

	limit := maxGroups
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, maxGroups)
	}

	groups, err := s.Store.Groups(r.Context(), kind, state, limit)
	if err != nil {
		s.logger().Error("read merge groups", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	// An explicit empty list rather than a null, so the client has one shape to
	// render and "none" is not a special case in the browser.
	if groups == nil {
		groups = []db.MergeGroup{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
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
func (s *Server) handleDismissMerge(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	w.WriteHeader(http.StatusNoContent)
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
