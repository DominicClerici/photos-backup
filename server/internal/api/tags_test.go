package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// fakeML is photo-ml as the tag cleanup uses it: a judge and a text encoder.
//
// It also records how many words arrived in each request, because the batching
// against PHOTO_ML_MAX_BATCH is a real property of these handlers — a slice of a
// hundred and twenty words has to reach the service as four requests rather
// than as one 413.
type fakeML struct {
	*httptest.Server
	junk   map[string]bool
	sizes  []int
	failed bool
}

func newFakeML(t *testing.T, junk ...string) *fakeML {
	f := &fakeML{junk: map[string]bool{}}
	for _, word := range junk {
		f.junk[word] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /triage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Words []string `json:"words"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if f.failed {
			http.Error(w, `{"detail":"the card went away"}`, http.StatusServiceUnavailable)
			return
		}
		f.sizes = append(f.sizes, len(req.Words))

		results := make([]map[string]any, len(req.Words))
		for i, word := range req.Words {
			score := 0.05
			if f.junk[word] {
				score = 0.97
			}
			results[i] = map[string]any{"word": word, "junk": f.junk[word], "score": score}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": db.CaptionModel, "results": results})
	})
	mux.HandleFunc("POST /embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Texts []string `json:"texts"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.sizes = append(f.sizes, len(req.Texts))

		vectors := make([][]float32, len(req.Texts))
		for i := range req.Texts {
			v := make([]float32, db.VisionDim)
			// One axis per word, so nothing is near anything: what these tests
			// are about is the plumbing, and the clustering itself is measured
			// against real vectors in internal/db.
			v[i%db.VisionDim] = 1
			vectors[i] = v
		}
		json.NewEncoder(w).Encode(map[string]any{
			"model": db.VisionModel, "dim": db.VisionDim,
			"normalized": true, "vectors": vectors,
		})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// intern puts words in the vocabulary without a photograph behind them. The
// cleanup is about the words; what they are attached to is internal/db's test.
func intern(t *testing.T, h *harness, words ...string) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for _, word := range words {
		var id int64
		err := h.store.Pool().QueryRow(context.Background(),
			`insert into tags (name) values ($1) returning id`, word).Scan(&id)
		if err != nil {
			t.Fatalf("intern %q: %v", word, err)
		}
		ids[word] = id
	}
	return ids
}

type tagCountsBody struct {
	Untriaged  int64 `json:"untriaged"`
	Unreviewed int64 `json:"unreviewed"`
	Junk       int64 `json:"junk"`
	Kept       int64 `json:"kept"`
	Unembedded int64 `json:"unembedded"`
	Folded     int64 `json:"folded"`
}

// The pass, and the contract the page's loop is written against: call again
// while there is anything left, and every call is a few seconds.
func TestTriagePassIsABoundedResumableSlice(t *testing.T) {
	h := newHarness(t)
	ml := newFakeML(t, "login", "result")
	h.srv.ML = mlclient.New(ml.URL)

	words := make([]string, 0, mlclient.MaxBatch+5)
	for i := range cap(words) {
		words = append(words, fmt.Sprintf("word%02d", i))
	}
	intern(t, h, append(words, "login", "result")...)

	res := h.postJSON(t, "/v1/tags/triage", `{}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("triage = %s", res.Status)
	}
	var out struct {
		Triaged int64         `json:"triaged"`
		Counts  tagCountsBody `json:"counts"`
	}
	decode(t, res, &out)

	if out.Triaged != int64(len(words)+2) {
		t.Errorf("triaged = %d, want all %d words: the slice is bigger than this vocabulary", out.Triaged, len(words)+2)
	}
	if out.Counts.Untriaged != 0 {
		t.Errorf("untriaged = %d after a pass that covered everything", out.Counts.Untriaged)
	}
	if out.Counts.Junk != 2 || out.Counts.Kept != int64(len(words)) {
		t.Errorf("counts = %+v, want two junk", out.Counts)
	}
	// Nobody has read any of it yet, which is the whole reason approving is a
	// separate button.
	if out.Counts.Unreviewed != int64(len(words)+2) {
		t.Errorf("unreviewed = %d, want every verdict waiting to be read", out.Counts.Unreviewed)
	}

	// photo-ml bounds every list route, so the slice arrived in batches.
	for _, size := range ml.sizes {
		if size > mlclient.MaxBatch {
			t.Errorf("a request carried %d words; photo-ml's limit is %d", size, mlclient.MaxBatch)
		}
	}
	if len(ml.sizes) < 2 {
		t.Errorf("%d requests for %d words, want the batch bound to have split them", len(ml.sizes), len(words)+2)
	}

	// And a second call has nothing to do, rather than judging the same words
	// again: `triaged_at is null` is the resume point.
	out.Triaged = 0
	decode(t, h.postJSON(t, "/v1/tags/triage", `{}`), &out)
	if out.Triaged != 0 {
		t.Errorf("a second pass judged %d words that had already been judged", out.Triaged)
	}
}

// PROJECT.md §4: photo-ml is optional forever. Without it the cleanup cannot
// start, and it says so rather than answering an empty list.
func TestTagPassesSayWhenPhotoMLIsMissing(t *testing.T) {
	h := newHarness(t)
	intern(t, h, "dog")

	for _, path := range []string{"/v1/tags/triage", "/v1/tags/embed"} {
		res := h.postJSON(t, path, `{}`)
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s = %s, want 503", path, res.Status)
		}
		var body struct {
			Error string `json:"error"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if !strings.Contains(body.Error, "photo-ml") {
			t.Errorf("%s said %q, which does not name what is missing", path, body.Error)
		}
	}

	// And everything that does not need it goes on working, which is the point.
	if res := h.get(t, "/v1/tags"); res.StatusCode != http.StatusOK {
		t.Errorf("the counts = %s with no photo-ml", res.Status)
	}
	if res := h.get(t, "/v1/tags/proposals"); res.StatusCode != http.StatusOK {
		t.Errorf("the proposals = %s with no photo-ml", res.Status)
	}
}

// A pass that dies halfway keeps what it learned, because the whole design is
// resumable: the alternative is a service restart during a triage costing the
// slice that was already judged.
func TestTriageKeepsWhatItLearnedBeforeAFailure(t *testing.T) {
	h := newHarness(t)
	ml := newFakeML(t)
	ml.failed = true
	h.srv.ML = mlclient.New(ml.URL)
	intern(t, h, "dog", "beach")

	res := h.postJSON(t, "/v1/tags/triage", `{}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("triage against a failing service = %s, want 200 with nothing done", res.Status)
	}
	var out struct {
		Triaged int64         `json:"triaged"`
		Counts  tagCountsBody `json:"counts"`
	}
	decode(t, res, &out)
	if out.Triaged != 0 || out.Counts.Untriaged != 2 {
		t.Errorf("out = %+v, want nothing judged and both words still waiting", out)
	}
}

// The two review lists, and the third state that is on neither of them.
func TestTagWordsAreTwoListsAndAMergeIsOnNeither(t *testing.T) {
	h := newHarness(t)
	ids := intern(t, h, "dog", "puppy", "login")

	h.postJSON(t, "/v1/tags/judge", fmt.Sprintf(`{"ids":[%d],"junk":true}`, ids["login"]))
	h.postJSON(t, "/v1/tags/merge",
		fmt.Sprintf(`{"canonical":%d,"members":[%d]}`, ids["dog"], ids["puppy"]))

	var kept struct {
		Words []db.TagWord `json:"words"`
		Total int64        `json:"total"`
	}
	decode(t, h.get(t, "/v1/tags/words"), &kept)
	if len(kept.Words) != 1 || kept.Words[0].Name != "dog" {
		t.Errorf("kept = %+v, want dog alone: puppy has been answered", kept.Words)
	}

	var junk struct {
		Words []db.TagWord `json:"words"`
	}
	decode(t, h.get(t, "/v1/tags/words?junk=1"), &junk)
	if len(junk.Words) != 1 || junk.Words[0].Name != "login" {
		t.Errorf("junk = %+v, want login alone", junk.Words)
	}

	var merged struct {
		Groups []db.TagProposal `json:"groups"`
	}
	decode(t, h.get(t, "/v1/tags/merged"), &merged)
	if len(merged.Groups) != 1 || merged.Groups[0].Canonical.Name != "dog" ||
		len(merged.Groups[0].Members) != 1 || merged.Groups[0].Members[0].Name != "puppy" {
		t.Fatalf("merged log = %+v, want puppy under dog", merged.Groups)
	}

	// The undo, which is the whole reason the merge is one column.
	h.postJSON(t, "/v1/tags/unmerge", fmt.Sprintf(`{"ids":[%d]}`, ids["puppy"]))
	kept.Words = nil
	decode(t, h.get(t, "/v1/tags/words"), &kept)
	if kept.Total != 2 {
		t.Errorf("kept = %d words after an unmerge, want dog and puppy", kept.Total)
	}
}

// A member unticked before accepting the rest is a rejection, and it has to be
// recorded with the merge or the next clustering run proposes the group again.
func TestMergeRecordsTheMembersLeftOutOfIt(t *testing.T) {
	h := newHarness(t)
	ids := intern(t, h, "mountains", "mountain", "mountaineering")

	res := h.postJSON(t, "/v1/tags/merge", fmt.Sprintf(
		`{"canonical":%d,"members":[%d],"rejected":[%d]}`,
		ids["mountains"], ids["mountain"], ids["mountaineering"]))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("merge = %s", res.Status)
	}
	var out db.TagMerge
	decode(t, res, &out)
	if out.Merged != 1 || out.Rejected != 1 {
		t.Errorf("merge = %+v, want one folded and one rejected", out)
	}

	var blocked bool
	err := h.store.Pool().QueryRow(context.Background(), `
		select exists (select 1 from tag_merge_blocks
		  where tag_id = least($1::bigint, $2::bigint)
		    and other_id = greatest($1::bigint, $2::bigint))`,
		ids["mountains"], ids["mountaineering"]).Scan(&blocked)
	if err != nil {
		t.Fatalf("read the blocked pairs: %v", err)
	}
	if !blocked {
		t.Error("the unticked member was not recorded as rejected, so it will be proposed again")
	}
}

// Merging into a word that has itself been merged is refused, because
// canonical_id is resolved one hop everywhere it is read.
func TestMergeIntoAFoldedWordIsRefused(t *testing.T) {
	h := newHarness(t)
	ids := intern(t, h, "dog", "puppy", "doggo")

	h.postJSON(t, "/v1/tags/merge", fmt.Sprintf(`{"canonical":%d,"members":[%d]}`, ids["dog"], ids["puppy"]))
	res := h.postJSON(t, "/v1/tags/merge", fmt.Sprintf(`{"canonical":%d,"members":[%d]}`, ids["puppy"], ids["doggo"]))
	if res.StatusCode != http.StatusConflict {
		t.Errorf("merging into a folded word = %s, want 409", res.Status)
	}
}
