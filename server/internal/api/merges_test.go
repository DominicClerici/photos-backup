package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/merge"
)

// seedGroup uploads n distinct assets and records them as one group, returning
// the group id and the asset ids in the order they were given.
func seedGroup(t *testing.T, h *harness, kind string, n int) (string, []string) {
	t.Helper()

	ids := make([]string, n)
	for i := range n {
		body := []byte(fmt.Sprintf("copy-%d-of-a-photograph", i))
		resp := h.upload(t, body, map[string]string{
			"X-Photo-Filename": fmt.Sprintf("IMG_%04d.HEIC", i),
			"X-Photo-Local-Id": fmt.Sprintf("local-%d", i),
		})
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("upload %d: status %d", i, resp.StatusCode)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode upload %d: %v", i, err)
		}
		ids[i] = out.ID
	}

	if _, err := h.store.RecordGroups(context.Background(),
		[]merge.Group{{Kind: kind, IDs: ids}}); err != nil {
		t.Fatalf("RecordGroups: %v", err)
	}

	groups, err := h.store.Groups(context.Background(), db.MergeQuery{Kind: kind, State: db.MergePending, Limit: 10})
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("%d pending groups, want 1", len(groups))
	}
	return groups[0].ID, ids
}

func decodeGroups(t *testing.T, resp *http.Response) []db.MergeGroup {
	t.Helper()
	var out struct {
		Groups []db.MergeGroup `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode groups: %v", err)
	}
	return out.Groups
}

func TestMergeGroupsListsThePendingReview(t *testing.T) {
	h := newHarness(t)
	_, ids := seedGroup(t, h, merge.KindDuplicate, 3)

	resp := h.get(t, "/v1/merges/groups")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	groups := decodeGroups(t, resp)
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1", len(groups))
	}
	if len(groups[0].Members) != len(ids) {
		t.Fatalf("%d members, want %d", len(groups[0].Members), len(ids))
	}
	for i, m := range groups[0].Members {
		if m.AssetID != ids[i] {
			t.Errorf("member %d is %s, want %s", i, m.AssetID, ids[i])
		}
		if m.Filename == "" {
			t.Errorf("member %d has no filename; the page draws one", i)
		}
	}
}

// Empty is a list, not a null. The client has one shape to render and "none" is
// not a special case in the browser.
func TestMergeGroupsAnswersAnEmptyListRatherThanNull(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/v1/merges/groups")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["groups"]) != "[]" {
		t.Errorf("groups = %s, want []", raw["groups"])
	}
}

func TestMergeGroupsRefusesAnUnknownKindOrState(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{
		"/v1/merges/groups?kind=nonsense",
		"/v1/merges/groups?state=nonsense",
		"/v1/merges/groups?limit=0",
		"/v1/merges/groups?limit=-4",
		"/v1/merges/groups?limit=lots",
	} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, resp.StatusCode)
		}
	}
}

// The limit is clamped rather than trusted: a group draws every member as a
// thumbnail, so this bounds how many images a browser is asked to hold.
func TestMergeGroupsClampsTheLimit(t *testing.T) {
	h := newHarness(t)
	seedGroup(t, h, merge.KindDuplicate, 2)

	resp := h.get(t, "/v1/merges/groups?limit=100000")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := len(decodeGroups(t, resp)); got != 1 {
		t.Errorf("%d groups, want 1", got)
	}
}

func TestMergeKeepsTheChosenCopy(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindDuplicate, 3)

	// Deliberately not the first, which is what the page preselects. The
	// endpoint takes an instruction, not a suggestion.
	resp := h.postJSON(t, "/v1/merges/"+group+"/merge", `{"keeper":"`+ids[2]+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result db.MergeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Keeper != ids[2] {
		t.Errorf("Keeper = %s, want %s", result.Keeper, ids[2])
	}
	if result.Trashed != 2 {
		t.Errorf("Trashed = %d, want 2", result.Trashed)
	}
	if result.Batch == "" {
		t.Error("no batch came back; there is nothing to undo with")
	}

	ctx := context.Background()
	kept, err := h.store.Asset(ctx, ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if kept.DeletedAt != nil {
		t.Error("the chosen copy was trashed")
	}
	for _, id := range ids[:2] {
		gone, err := h.store.Asset(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if gone.DeletedAt == nil {
			t.Errorf("copy %s is still in the library", id)
		}
	}
}

// The undo is the ordinary one. A merge is a delete with a batch, and the
// toast's Undo restores exactly the rows that operation touched.
func TestAMergeIsUndoneByTheOrdinaryRestore(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindDuplicate, 3)

	resp := h.postJSON(t, "/v1/merges/"+group+"/merge", `{"keeper":"`+ids[0]+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge: status %d", resp.StatusCode)
	}
	var result db.MergeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}

	undo := h.postJSON(t, "/v1/trash/restore", `{"batch":"`+result.Batch+`"}`)
	if undo.StatusCode != http.StatusOK {
		t.Fatalf("restore: status %d", undo.StatusCode)
	}

	for _, id := range ids {
		a, err := h.store.Asset(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt != nil {
			t.Errorf("copy %s did not come back", id)
		}
	}
}

func TestMergeRequiresAKeeper(t *testing.T) {
	h := newHarness(t)
	group, _ := seedGroup(t, h, merge.KindDuplicate, 2)

	for _, body := range []string{`{}`, `{"keeper":""}`} {
		resp := h.postJSON(t, "/v1/merges/"+group+"/merge", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("merge with body %s = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestMergeRefusesAKeeperFromOutsideTheGroup(t *testing.T) {
	h := newHarness(t)
	group, _ := seedGroup(t, h, merge.KindDuplicate, 2)

	resp := h.postJSON(t, "/v1/merges/"+group+"/merge",
		`{"keeper":"7c8b1a2d-0000-4000-8000-000000000000"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A set of video segments has no copy among them to keep — the thing that
// survives is a file that does not exist until the worker builds it. Electing
// one ten-second piece would throw away the rest of the minute.
func TestMergeRefusesToResolveASegmentGroupByHand(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindSegments, 3)

	resp := h.postJSON(t, "/v1/merges/"+group+"/merge", `{"keeper":"`+ids[0]+`"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	for _, id := range ids {
		a, err := h.store.Asset(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt != nil {
			t.Errorf("piece %s was trashed by a refused request", id)
		}
	}
}

func TestMergeRefusesTheSecondClick(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindDuplicate, 3)

	first := h.postJSON(t, "/v1/merges/"+group+"/merge", `{"keeper":"`+ids[0]+`"}`)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first merge: status %d", first.StatusCode)
	}
	second := h.postJSON(t, "/v1/merges/"+group+"/merge", `{"keeper":"`+ids[1]+`"}`)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second merge = %d, want 409", second.StatusCode)
	}

	kept, err := h.store.Asset(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if kept.DeletedAt != nil {
		t.Error("the second click trashed the copy the first one kept")
	}
}

func TestMergeReportsAnUnknownGroup(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"7c8b1a2d-0000-4000-8000-000000000000", "not-a-uuid"} {
		resp := h.postJSON(t, "/v1/merges/"+id+"/merge", `{"keeper":"x"}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("merge on %q = %d, want 404", id, resp.StatusCode)
		}
	}
}

func TestDismissMovesAGroupOutOfTheReview(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindDuplicate, 3)

	resp := h.postJSON(t, "/v1/merges/"+group+"/dismiss", `{}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if got := decodeGroups(t, h.get(t, "/v1/merges/groups")); len(got) != 0 {
		t.Errorf("%d groups still pending after a dismissal", len(got))
	}
	if got := decodeGroups(t, h.get(t, "/v1/merges/groups?state=dismissed")); len(got) != 1 {
		t.Errorf("%d dismissed groups, want 1", len(got))
	}

	// Nothing was deleted. A dismissal says these are different photographs.
	for _, id := range ids {
		a, err := h.store.Asset(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt != nil {
			t.Errorf("%s was trashed by a dismissal", id)
		}
	}

	if again := h.postJSON(t, "/v1/merges/"+group+"/dismiss", `{}`); again.StatusCode != http.StatusConflict {
		t.Errorf("dismissing twice = %d, want 409", again.StatusCode)
	}
}

func TestUndoPutsAJoinedRecordingBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	group, ids := seedGroup(t, h, merge.KindSegments, 3)

	// Stand in for the worker: the joined recording is an asset that did not
	// exist when the group was found.
	joinedResp := h.upload(t, []byte("the whole minute"), map[string]string{
		"X-Photo-Filename": "joined.mp4",
		"X-Photo-Local-Id": "local-joined",
	})
	var joined struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(joinedResp.Body).Decode(&joined); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.MergeSegments(ctx, group, joined.ID); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}

	resp := h.postJSON(t, "/v1/merges/"+group+"/undo", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out db.Unmerged
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Restored != len(ids) {
		t.Errorf("Restored = %d, want %d", out.Restored, len(ids))
	}

	for _, id := range ids {
		a, err := h.store.Asset(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.DeletedAt != nil {
			t.Errorf("piece %s did not come back", id)
		}
	}
	gone, err := h.store.Asset(ctx, joined.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.DeletedAt == nil {
		t.Error("the joined recording is still in the library beside its own pieces")
	}
}

func TestUndoReportsAGroupThatWasNeverMerged(t *testing.T) {
	h := newHarness(t)
	group, _ := seedGroup(t, h, merge.KindDuplicate, 2)

	if resp := h.postJSON(t, "/v1/merges/"+group+"/undo", `{}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMergeCountsSaysHowMuchOfTheLibraryItSaw(t *testing.T) {
	h := newHarness(t)
	seedGroup(t, h, merge.KindDuplicate, 3)

	resp := h.get(t, "/v1/merges")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var counts db.MergeCounts
	if err := json.NewDecoder(resp.Body).Decode(&counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if counts.PendingDuplicates != 1 {
		t.Errorf("PendingDuplicates = %d, want 1", counts.PendingDuplicates)
	}
	if counts.DuplicateItems != 3 {
		t.Errorf("DuplicateItems = %d, want 3", counts.DuplicateItems)
	}
	// Nothing has been signed in this harness, and the card has to be able to
	// say that rather than reporting "no duplicates" as though it had looked.
	if counts.Coverage.Assets != 3 {
		t.Errorf("Coverage.Assets = %d, want 3", counts.Coverage.Assets)
	}
	if counts.Coverage.Signed != 0 {
		t.Errorf("Coverage.Signed = %d, want 0", counts.Coverage.Signed)
	}
}

// A server running with WORKER_DISABLED can still read the review and still
// resolve a group. What it cannot do is look for more, and it says so rather
// than reporting a scan that found nothing.
func TestScanAnswersHonestlyWithNoWorker(t *testing.T) {
	h := newHarness(t)
	h.srv.Scan = nil

	resp := h.postJSON(t, "/v1/merges/scan", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestScanReturnsWhatItFound(t *testing.T) {
	h := newHarness(t)
	h.srv.Scan = func(context.Context) (merge.ScanResult, error) {
		return merge.ScanResult{Segments: 2, Duplicates: 5, Queued: 2, Signed: 40, Assets: 100}, nil
	}

	resp := h.postJSON(t, "/v1/merges/scan", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out merge.ScanResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Duplicates != 5 || out.Segments != 2 || out.Signed != 40 {
		t.Errorf("result = %+v, want the scan's own numbers", out)
	}
}

func TestScanReportsItsOwnFailure(t *testing.T) {
	h := newHarness(t)
	h.srv.Scan = func(context.Context) (merge.ScanResult, error) {
		return merge.ScanResult{}, errors.New("the disk fell off")
	}

	resp := h.postJSON(t, "/v1/merges/scan", `{}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// The plaintext listener is the one the browser gallery reaches, so the review
// has to work over it — and it is loopback-only, which is what makes that
// acceptable. Same exposure as the trash endpoints beside it.
func TestMergeRoutesAreServedToTheGallery(t *testing.T) {
	h := newHarness(t)
	seedGroup(t, h, merge.KindDuplicate, 2)
	gallery := h.gallery(t)

	resp, err := gallery.Client().Get(gallery.URL + "/v1/merges/groups")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := decodeGroups(t, resp); len(got) != 1 {
		t.Errorf("%d groups, want 1", len(got))
	}
}

// failJoin puts the group's queued join into the state a worker that gave up
// leaves it in, and writes a rejected join to disk under the group.
func failJoin(t *testing.T, h *harness, group, head, message string, preview []byte) {
	t.Helper()
	ctx := context.Background()

	if err := jobs.Enqueue(ctx, h.store.Pool(), jobs.KindMerge, head); err != nil {
		t.Fatalf("enqueue the join: %v", err)
	}
	if _, err := h.store.Pool().Exec(ctx, `
		update jobs set state = 'failed', attempts = 5, last_error = $3, updated_at = now()
		where kind = $1 and asset_id = $2::uuid`, jobs.KindMerge, head, message); err != nil {
		t.Fatalf("fail the join: %v", err)
	}
	if preview == nil {
		return
	}

	g, err := h.store.Group(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	store := derivstore.New(h.derivRoot)
	if err := store.Write(g.Fingerprint, derivstore.JoinPreview, func(w io.Writer) error {
		_, err := w.Write(preview)
		return err
	}); err != nil {
		t.Fatalf("write the rejected join: %v", err)
	}
}

// The failed half of the joined-recordings tab: groups that are still pending,
// carrying the error that stopped them and the offer of the file to watch.
func TestMergeGroupsListsTheJoinsThatFailed(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindSegments, 3)
	const why = "join: 3 parts totalling 28.643s came out as 28.797s; refusing to archive that"
	failJoin(t, h, group, ids[0], why, []byte("not really an mp4"))

	resp := h.get(t, "/v1/merges/groups?kind=video-segments&state=failed")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Groups []struct {
			db.MergeGroup
			Preview bool `json:"preview"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode groups: %v", err)
	}
	if len(out.Groups) != 1 {
		t.Fatalf("%d failed groups, want 1", len(out.Groups))
	}
	got := out.Groups[0]
	if got.Failure == nil || got.Failure.Error != why {
		t.Errorf("failure = %+v, want the error verbatim", got.Failure)
	}
	if !got.Preview {
		t.Error("preview = false, so the page would not offer the file that is right there")
	}
}

// The one video this server serves that belongs to no asset.
func TestJoinPreviewServesTheRefusedFile(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindSegments, 3)
	body := []byte("the minute of video nobody archived")
	failJoin(t, h, group, ids[0], "did not add up", body)

	resp := h.get(t, "/v1/merges/"+group+"/preview")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	served, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(served) != string(body) {
		t.Errorf("served %q, want the refused join", served)
	}

	// A group with nothing kept for it is a 404 rather than an empty video.
	other, otherIDs := seedGroup(t, h, merge.KindSegments, 2)
	failJoin(t, h, other, otherIDs[0], "part missing from the blob store", nil)
	if resp := h.get(t, "/v1/merges/"+other+"/preview"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d for a group with no kept join, want 404", resp.StatusCode)
	}
}

// Approving takes an entry off the list and out of the count, and the same
// click puts it back.
func TestApproveAndUnapproveAJoinedRecording(t *testing.T) {
	h := newHarness(t)
	group, _ := seedGroup(t, h, merge.KindSegments, 3)

	// Resolved the way the worker resolves one: the keeper is a file that did
	// not exist when the group was found, and is not a member of it.
	resp := h.upload(t, []byte("the joined recording"), map[string]string{
		"X-Photo-Filename": "2018-10-07_piece0-joined.mp4",
		"X-Photo-Local-Id": "local-joined",
	})
	var made struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&made); err != nil {
		t.Fatalf("decode the joined upload: %v", err)
	}
	if _, err := h.store.MergeSegments(context.Background(), group, made.ID); err != nil {
		t.Fatalf("MergeSegments: %v", err)
	}

	if groups := decodeGroups(t, h.get(t, "/v1/merges/groups?kind=video-segments&state=merged")); len(groups) != 1 {
		t.Fatalf("%d merged groups before approving, want 1", len(groups))
	}

	if resp := h.postJSON(t, "/v1/merges/"+group+"/approve", "{}"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("approve: status = %d, want 204", resp.StatusCode)
	}
	if groups := decodeGroups(t, h.get(t, "/v1/merges/groups?kind=video-segments&state=merged")); len(groups) != 0 {
		t.Errorf("%d merged groups after approving, want none on the default list", len(groups))
	}
	groups := decodeGroups(t, h.get(t, "/v1/merges/groups?kind=video-segments&state=merged&approved=true"))
	if len(groups) != 1 || groups[0].ApprovedAt == nil {
		t.Fatalf("show-approved returned %d groups, want the approved one with its date", len(groups))
	}

	if resp := h.postJSON(t, "/v1/merges/"+group+"/unapprove", "{}"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unapprove: status = %d, want 204", resp.StatusCode)
	}
	if groups := decodeGroups(t, h.get(t, "/v1/merges/groups?kind=video-segments&state=merged")); len(groups) != 1 {
		t.Errorf("%d merged groups after unapproving, want it back", len(groups))
	}

	// A group that was never merged has nothing to have been read.
	pending, _ := seedGroup(t, h, merge.KindSegments, 2)
	if resp := h.postJSON(t, "/v1/merges/"+pending+"/approve", "{}"); resp.StatusCode != http.StatusConflict {
		t.Errorf("approving a pending group: status = %d, want 409", resp.StatusCode)
	}
}

// Overruling the duration check queues the join again with the objection
// disabled, and says so on the group.
func TestForceJoinQueuesTheWorkAgain(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindSegments, 3)
	failJoin(t, h, group, ids[0], "did not add up", []byte("kept"))

	resp := h.postJSON(t, "/v1/merges/"+group+"/force", "{}")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	g, err := h.store.Group(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Forced {
		t.Error("the override was not recorded, so the next attempt would refuse again")
	}

	var state string
	if err := h.store.Pool().QueryRow(context.Background(),
		`select state from jobs where kind = $1 and asset_id = $2::uuid`,
		jobs.KindMerge, ids[0]).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(jobs.StatePending) {
		t.Errorf("job is %s, want pending", state)
	}

	// A pile of duplicates is not something to join.
	dupes, _ := seedGroup(t, h, merge.KindDuplicate, 2)
	if resp := h.postJSON(t, "/v1/merges/"+dupes+"/force", "{}"); resp.StatusCode != http.StatusConflict {
		t.Errorf("forcing a duplicate group: status = %d, want 409", resp.StatusCode)
	}
}

// Refusing a join takes it off the queue with it, so the status page stops
// reporting a job that gave up on work nobody wants done.
func TestDismissingAFailedJoinClearsItsJobAndItsFile(t *testing.T) {
	h := newHarness(t)
	group, ids := seedGroup(t, h, merge.KindSegments, 3)
	failJoin(t, h, group, ids[0], "did not add up", []byte("kept"))

	g, err := h.store.Group(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	if resp := h.postJSON(t, "/v1/merges/"+group+"/dismiss", "{}"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dismiss: status = %d, want 204", resp.StatusCode)
	}

	var left int
	if err := h.store.Pool().QueryRow(context.Background(),
		`select count(*)::int from jobs where kind = $1`, jobs.KindMerge).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d merge jobs left after the group was refused, want none", left)
	}
	if derivstore.New(h.derivRoot).Exists(g.Fingerprint, derivstore.JoinPreview) {
		t.Error("the refused join is still on disk after the group was refused")
	}
}
