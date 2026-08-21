package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// The tag cleanup's endpoints — ML_IMAGES.md §9.
//
// Eleven of them, which is more than any other feature here has, and the reason
// is that the cleanup is two passes with a review between each and its result.
// Four reads (the counts, the two review lists, the proposals and the log) and
// six writes, of which two are the passes themselves.
//
// The two passes are the interesting shape. Both call photo-ml over the whole
// vocabulary, both take longer than a request should be held open — a couple of
// minutes for the triage, seconds for the embedding — and neither is worth a
// job kind, a pool and a reconcile: they are typed by a person who is about to
// sit and read the result, they happen once per model generation, and nothing
// in the archive is waiting on them. So each is a *bounded slice*, and the page
// calls it in a loop until nothing is left. Every call is a few seconds, the
// resume point is a column with an index on it, and closing the browser halfway
// through loses nothing but the loop.

const (
	// triageChunk is how many words one POST judges. About six seconds of a 4B
	// on this card, which is short enough to hold a request open and long
	// enough that a three-thousand-word vocabulary is twenty-five calls rather
	// than a hundred.
	triageChunk = 120
	// embedChunk is the same idea against a far cheaper model: the text tower
	// gets through a whole vocabulary in about seven seconds, so the slice is
	// bigger and the loop is shorter.
	embedChunk = 512

	// maxTagGroups bounds one page of proposals, for the reason maxGroups
	// bounds the duplicate review: every member of every group is drawn with
	// photographs beside it, so this is a limit on what a browser is asked to
	// hold rather than on how many merges there are.
	maxTagGroups = 60
	// maxTagWords bounds one page of a review list. The lists are chips rather
	// than thumbnails, so they can be much longer.
	maxTagWords     = 500
	defaultTagWords = 200

	// tagSamples is how many photographs are drawn beside a word. Three is
	// enough to tell "doggo" from "dog" and few enough that a page of sixty
	// groups is not a thousand images.
	tagSamples = 3
)

// handleTagCounts is the status card, and the review screen's map of itself:
// which stage the cleanup is in and how much is left of it.
func (s *Server) handleTagCounts(w http.ResponseWriter, r *http.Request) {
	counts, err := s.Store.TagCleanupCounts(r.Context())
	if err != nil {
		s.logger().Error("count the tag vocabulary", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	// The number of merges waiting is a clustering run rather than a column, so
	// it is computed here rather than counted: about 240ms over this
	// vocabulary, once, for a card that is read once. Samples are deliberately
	// not fetched — nothing draws a thumbnail from this answer.
	out := tagCountsView{TagCounts: counts}
	groups, err := s.Store.TagProposals(r.Context(), db.TagProposalQuery{})
	if err != nil {
		// The counts are worth answering without it. A card that says how many
		// words there are and stays quiet about the merges is better than no
		// card, and the review screen asks for the proposals properly anyway.
		s.logger().Warn("count the merge proposals", "error", err)
	} else {
		out.Suggestions = len(groups)
	}
	writeJSON(w, http.StatusOK, out)
}

type tagCountsView struct {
	db.TagCounts
	// Suggestions is how many merges the current vocabulary would propose at
	// the default threshold. Zero either because everything is merged or
	// because nothing is embedded yet, which Unembedded distinguishes.
	Suggestions int `json:"suggestions"`
}

// handleTagWords serves one page of one of the two review lists.
//
// The list is chosen by a query flag against a closed pair rather than by two
// paths, because they are one collection seen from two sides and the page
// switches between them the way the merge review switches tabs.
func (s *Server) handleTagWords(w http.ResponseWriter, r *http.Request) {
	query := db.TagWordQuery{
		Junk:    boolParam(r, "junk"),
		Search:  r.URL.Query().Get("q"),
		Limit:   defaultTagWords,
		Samples: tagSamples,
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		query.Limit = min(n, maxTagWords)
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a row offset")
			return
		}
		query.Offset = n
	}

	words, matched, err := s.Store.TagWords(r.Context(), query)
	if err != nil {
		s.logger().Error("read the tag vocabulary", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"words": words, "total": matched})
}

// handleTagProposals clusters the vocabulary and answers with the groups.
//
// The threshold is a parameter because it is a control on the review screen,
// and it is a control because the useful range is narrow, high, and moves with
// the vocabulary — see db.DefaultTagSimilarity, which is where the measurements
// are written down. Storing the vectors is what makes dragging it affordable.
func (s *Server) handleTagProposals(w http.ResponseWriter, r *http.Request) {
	query := db.TagProposalQuery{
		Similarity: db.DefaultTagSimilarity,
		Limit:      maxTagGroups,
		Samples:    tagSamples,
	}
	if raw := r.URL.Query().Get("similarity"); raw != "" {
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || n <= 0 || n > 1 {
			writeError(w, http.StatusBadRequest, "similarity must be between 0 and 1")
			return
		}
		query.Similarity = n
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		query.Limit = min(n, maxTagGroups)
	}

	groups, err := s.Store.TagProposals(r.Context(), query)
	if err != nil {
		s.logger().Error("cluster the tag vocabulary", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	counts, err := s.Store.TagCleanupCounts(r.Context())
	if err != nil {
		s.logger().Error("count the tag vocabulary", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	// Unembedded travels with the groups because an empty list means two
	// different things — every word has been merged, or no word has a vector
	// yet — and a page that drew both as "nothing to do" would be hiding a
	// button somebody needs to press.
	writeJSON(w, http.StatusOK, map[string]any{
		"groups":     groups,
		"similarity": query.Similarity,
		"unembedded": counts.Unembedded,
	})
}

// handleMergedTags is the log of what has been folded, and the way out of it.
func (s *Server) handleMergedTags(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Store.MergedTags(r.Context(), 0)
	if err != nil {
		s.logger().Error("read the merged words", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// handleTagTriage judges the next slice of words, and says how many are left.
//
// The loop is the client's, and the contract is `remaining`: call again while
// it is above zero. Nothing here is idempotent in the strict sense — a second
// call judges different words — but it is *resumable*, which is the property
// that actually matters when the thing driving it is a browser tab.
func (s *Server) handleTagTriage(w http.ResponseWriter, r *http.Request) {
	if s.ML == nil {
		writeError(w, http.StatusServiceUnavailable,
			"photo-ml is not configured, so nothing can judge the vocabulary; the words are all still here and searchable")
		return
	}

	words, err := s.Store.UntriagedTags(r.Context(), triageChunk)
	if err != nil {
		s.logger().Error("read the words waiting to be judged", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	verdicts := make([]db.TagVerdict, 0, len(words))
	for chunk := range batches(words, mlclient.MaxBatch) {
		names := make([]string, len(chunk))
		for i, word := range chunk {
			names[i] = word.Name
		}
		judged, err := s.ML.Triage(r.Context(), names)
		if err != nil {
			// Whatever was judged before the failure is still worth writing:
			// the pass is resumable by construction, so a service that went
			// away halfway costs the rest of this slice and nothing else.
			s.logger().Warn("could not judge a batch of words", "error", err, "words", len(names))
			break
		}
		for i, v := range judged.Results {
			verdicts = append(verdicts, db.TagVerdict{ID: chunk[i].ID, Junk: v.Junk, Score: v.Score})
		}
	}

	applied, err := s.Store.PutTriage(r.Context(), verdicts)
	if err != nil {
		s.logger().Error("record the triage verdicts", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	s.answerWithCounts(w, r, map[string]any{"triaged": applied})
}

// handleTagEmbed embeds the next slice of kept words.
//
// The same bounded-slice shape as the triage above and against a much cheaper
// model, which is why the slice is four times the size. It is a separate call
// rather than the tail of the same one because the two passes answer to
// different reviews: what gets embedded depends on what survived the triage,
// and what survived the triage is a decision somebody makes in between.
func (s *Server) handleTagEmbed(w http.ResponseWriter, r *http.Request) {
	if s.ML == nil {
		writeError(w, http.StatusServiceUnavailable,
			"photo-ml is not configured, so the words cannot be compared with each other; merges already made are unaffected")
		return
	}

	words, err := s.Store.UnembeddedTags(r.Context(), embedChunk)
	if err != nil {
		s.logger().Error("read the words waiting to be embedded", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	var embedded int
	for chunk := range batches(words, mlclient.MaxBatch) {
		names := make([]string, len(chunk))
		ids := make([]int64, len(chunk))
		for i, word := range chunk {
			names[i], ids[i] = word.Name, word.ID
		}
		vectors, err := s.ML.EmbedTexts(r.Context(), names)
		if err != nil {
			s.logger().Warn("could not embed a batch of words", "error", err, "words", len(names))
			break
		}
		if len(vectors.Vectors) != len(names) {
			s.logger().Warn("photo-ml embedded the wrong number of words",
				"asked", len(names), "answered", len(vectors.Vectors))
			break
		}
		// The model the service actually ran, not the one that was asked for —
		// the same rule the vision job applies, so a service quietly holding a
		// different checkpoint writes vectors under its own name rather than
		// silently incomparable numbers under this one's.
		if err := s.Store.PutTagEmbeddings(r.Context(), vectors.Model, ids, vectors.Vectors); err != nil {
			s.logger().Error("store tag vectors", "error", err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		embedded += len(ids)
	}
	s.answerWithCounts(w, r, map[string]any{"embedded": embedded})
}

type judgeRequest struct {
	IDs []int64 `json:"ids"`
	// Junk is which way, and it is required rather than a toggle: the review
	// list is two lists and a request that only named words would be ambiguous
	// about which one they were being moved to.
	Junk bool `json:"junk"`
}

// handleJudgeTags is a person moving words between the two lists.
func (s *Server) handleJudgeTags(w http.ResponseWriter, r *http.Request) {
	var req judgeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required: say which words")
		return
	}

	changed, err := s.Store.JudgeTags(r.Context(), req.IDs, req.Junk)
	if err != nil {
		s.logger().Error("record a judgement on words", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	s.answerWithCounts(w, r, map[string]any{"judged": changed})
}

// handleApproveTriage signs the whole triage off, and makes the search index
// true in the same breath.
//
// The bulk rebuild lives here rather than after every verdict for the reason
// db.PutTriage explains: one pass over the library at the end, instead of most
// of one per chunk twenty-five times over.
func (s *Server) handleApproveTriage(w http.ResponseWriter, r *http.Request) {
	approved, reindexed, err := s.Store.ApproveTriage(r.Context())
	if err != nil {
		s.logger().Error("approve the triage", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	s.answerWithCounts(w, r, map[string]any{"approved": approved, "reindexed": reindexed})
}

type mergeTagsRequest struct {
	// Canonical is the word everything else becomes, and it is named rather
	// than inferred for the reason a duplicate merge names its keeper: the
	// proposal suggests the most-used member, the page preselects it, and
	// somebody can change it — so a request that named nothing would be
	// ambiguous between agreeing and forgetting.
	Canonical int64   `json:"canonical"`
	Members   []int64 `json:"members"`
	// Rejected is what was unticked before accepting the rest. Sent with the
	// merge rather than as a second call, because it is one decision: without
	// it the next clustering run proposes exactly the group just corrected.
	Rejected []int64 `json:"rejected,omitempty"`
}

// handleMergeTags folds a group of words into one.
func (s *Server) handleMergeTags(w http.ResponseWriter, r *http.Request) {
	var req mergeTagsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Canonical == 0 {
		writeError(w, http.StatusBadRequest, "canonical is required: say which word the others become")
		return
	}
	if len(req.Members) == 0 && len(req.Rejected) == 0 {
		writeError(w, http.StatusBadRequest, "members is required: say which words to merge")
		return
	}

	out, err := s.Store.MergeTags(r.Context(), req.Canonical, req.Members, req.Rejected)
	switch {
	case errors.Is(err, db.ErrNoSuchTag):
		writeError(w, http.StatusNotFound, "no such word")
		return
	case errors.Is(err, db.ErrTagFolded):
		writeError(w, http.StatusConflict,
			"that word has already been merged into another; undo that first, or merge into the word it became")
		return
	case err != nil:
		s.logger().Error("merge words", "error", err, "canonical", req.Canonical)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type tagIDsRequest struct {
	IDs []int64 `json:"ids"`
}

// handleDismissTagProposal records that a group of words are not one word.
func (s *Server) handleDismissTagProposal(w http.ResponseWriter, r *http.Request) {
	var req tagIDsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) < 2 {
		writeError(w, http.StatusBadRequest, "ids must name at least two words")
		return
	}

	blocked, err := s.Store.DismissTagProposal(r.Context(), req.IDs)
	if err != nil {
		s.logger().Error("dismiss a tag proposal", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": blocked})
}

// handleUnmergeTags puts folded words back.
//
// One column cleared, and every photograph carrying one of them re-indexed — so
// the undo is complete at the moment it is pressed rather than at the next time
// somebody runs `photobackup ml reindex`.
func (s *Server) handleUnmergeTags(w http.ResponseWriter, r *http.Request) {
	var req tagIDsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required: say which words to put back")
		return
	}

	restored, err := s.Store.UnmergeTags(r.Context(), req.IDs)
	if err != nil {
		s.logger().Error("undo a tag merge", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": restored})
}

// answerWithCounts sends what a write did, with the state of the cleanup
// hanging off it.
//
// Every write here changes at least two of the nine figures the review screen
// is laid out from — judging a word moves it between the lists and changes what
// the clustering would propose — so a client that had to ask again after each
// one would spend a request per click to draw a header. Folding them in makes
// the page's counts a fact about the write rather than about a moment shortly
// after it.
func (s *Server) answerWithCounts(w http.ResponseWriter, r *http.Request, out map[string]any) {
	counts, err := s.Store.TagCleanupCounts(r.Context())
	if err != nil {
		s.logger().Error("count the tag vocabulary", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out["counts"] = counts
	writeJSON(w, http.StatusOK, out)
}

// readJSON decodes a request body, answering 400 with the reason when it
// cannot.
func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body: "+err.Error())
		return false
	}
	return true
}

// batches walks a slice in fixed-size pieces.
//
// photo-ml bounds every list route at PHOTO_ML_MAX_BATCH, so a slice of a
// hundred and twenty words is four requests rather than one 413. The bound is
// the service's rather than this package's, which is why the size comes from
// mlclient.
func batches[T any](items []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(items); start += size {
			if !yield(items[start:min(start+size, len(items))]) {
				return
			}
		}
	}
}
