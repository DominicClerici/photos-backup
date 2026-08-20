package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// The whole path, end to end: an original arrives, Go decodes it into a
// rendition, the rendition goes out over HTTP, and a sentence and some words
// come back and land on three tables at once.
func TestDescribeWritesACaptionTagsAndTheSearchRow(t *testing.T) {
	ml := newFakeML(t)
	ml.captions = []string{"a golden retriever on a beach"}
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindDescribe)

	if got := h.caption(t, asset.ID); got != "a golden retriever on a beach" {
		t.Errorf("caption = %q", got)
	}
	if got := h.tags(t, asset.ID); len(got) != 2 {
		t.Errorf("tags = %v, want the two the model wrote", got)
	}
	// The tsvector is written in the same transaction, because a photograph
	// findable by "dog" that cannot say why is a half-published result.
	if !h.searchable(t, asset.ID, "retriever") {
		t.Error("the caption did not reach the full-text index")
	}
	if len(ml.described) != 1 || len(ml.described[0]) != 1 {
		t.Fatalf("sent %v, want one image in one request", ml.described)
	}
	if state := h.jobState(t, asset.ID, jobs.KindDescribe); state != string(jobs.StateDone) {
		t.Errorf("describe job state = %q, want done", state)
	}
}

// A clip is not one picture, and it is not six either. Three frames go to the
// captioner — the expensive pass in the system — and come back as one asset's
// worth of words, so a video that opens on a beach and ends in a restaurant is
// findable as both.
func TestDescribeSamplesThreeFramesOfAVideoAndFoldsThem(t *testing.T) {
	ml := newFakeML(t)
	ml.captions = []string{"a beach", "a road", "a restaurant"}
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindDescribe)

	if len(ml.described) != 1 || len(ml.described[0]) != describeFrames {
		t.Fatalf("sent %d requests of %v images, want %d frames in one",
			len(ml.described), lengths(ml.described), describeFrames)
	}

	caption := h.caption(t, asset.ID)
	for _, want := range []string{"a beach", "a restaurant"} {
		if !strings.Contains(caption, want) {
			t.Errorf("caption %q lost %q; a clip is as findable as its best moment", caption, want)
		}
	}
	// One row per asset, not one per frame: asset_descriptions is keyed by
	// (asset_id, model) and the folding happens on this side of the socket.
	if h.captionRows(t, asset.ID) != 1 {
		t.Error("a video produced more than one caption row")
	}
}

// A static clip produces the same sentence three times, and three copies of it
// in the tsvector would weight that video as though somebody had described it
// very carefully.
func TestARepeatedCaptionIsStoredOnce(t *testing.T) {
	ml := newFakeML(t)
	ml.captions = []string{"a wall", "a wall", "a wall"}
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindDescribe)

	if got := h.caption(t, asset.ID); got != "a wall" {
		t.Errorf("caption = %q, want it said once", got)
	}
}

func TestOCRStoresWhatThePhotographSays(t *testing.T) {
	ml := newFakeML(t)
	ml.text = []string{"TOTAL $42.50"}
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindOCR)

	if got := h.ocr(t, asset.ID); got != "TOTAL $42.50" {
		t.Errorf("recognised text = %q", got)
	}
	if !h.searchable(t, asset.ID, "total") {
		t.Error("the recognised text did not reach the full-text index")
	}
}

// The difference between "there is no text in this photograph" and "nobody has
// looked". Without the row, the backfill would offer ninety percent of the
// library again on every run.
func TestAPhotographWithNoTextStillGetsARow(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindOCR)

	var rows int
	if err := h.store.Pool().QueryRow(context.Background(),
		"select count(*) from asset_ocr where asset_id = $1::uuid", asset.ID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("stored %d rows for a wordless photograph, want 1 saying so", rows)
	}
}

// A burned-in Snapchat caption is on every frame of the clip by construction,
// and storing it three times would make that video rank as though the words
// were three times as present as they are.
func TestRepeatedTextAcrossFramesIsStoredOnce(t *testing.T) {
	ml := newFakeML(t)
	ml.text = []string{"the mountain", "the mountain", "the mountain\n8m ago"}
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindOCR)

	if got := h.ocr(t, asset.ID); got != "the mountain\n8m ago" {
		t.Errorf("recognised text = %q, want each line once", got)
	}
}

// A caption is the most legible description of a photograph this server ever
// writes. Writing one for something in the vault would be recording in plain
// English the thing the vault exists to stop it knowing.
func TestNothingIsWrittenAboutAVaultedPhotograph(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	if _, err := h.store.Pool().Exec(context.Background(),
		"update assets set vault = 'hidden' where id = $1::uuid", asset.ID); err != nil {
		t.Fatalf("hide: %v", err)
	}

	h.claimAndRun(t, jobs.KindDescribe)
	h.claimAndRun(t, jobs.KindOCR)

	if len(ml.described) != 0 || len(ml.recognised) != 0 {
		t.Errorf("sent a hidden photograph to photo-ml: %v %v", ml.described, ml.recognised)
	}
	for _, kind := range []jobs.Kind{jobs.KindDescribe, jobs.KindOCR} {
		if state := h.jobState(t, asset.ID, kind); state != string(jobs.StateDone) {
			t.Errorf("%s state = %q, want done: there is genuinely nothing left to do", kind, state)
		}
	}
}

// photo-ml going away mid-backfill must cost the queue nothing, and that has to
// be true of the four-hour pass as well as the fifteen-minute one — five swings
// at a closed socket per asset would park the library as permanently failed.
func TestDescribeAlsoDefersWhenPhotoMLGoesAway(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	ml.Close()
	job := h.claimAndRun(t, jobs.KindDescribe)

	state, attempts := h.jobStateAndAttempts(t, job.ID)
	if state != string(jobs.StatePending) {
		t.Fatalf("state = %q, want pending — the work was never attempted", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0: the job never reached the bytes", attempts)
	}
}

// The mlprep job queues all three passes at the moment the file they need
// exists, so a photograph arriving from a phone is described a minute later.
// The library-wide backfill is a command somebody types; this is not.
func TestMLPrepQueuesAllThreePasses(t *testing.T) {
	h := newHarness(t).withML(newFakeML(t).URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	for _, kind := range []jobs.Kind{jobs.KindVision, jobs.KindOCR, jobs.KindDescribe} {
		if state := h.jobState(t, asset.ID, kind); state != string(jobs.StatePending) {
			t.Errorf("%s state = %q, want pending once the renditions are on disk", kind, state)
		}
	}
}

func (h *harness) caption(t *testing.T, assetID string) string {
	t.Helper()
	var caption string
	err := h.store.Pool().QueryRow(context.Background(),
		"select caption from asset_descriptions where asset_id = $1::uuid and model = $2",
		assetID, db.CaptionModel).Scan(&caption)
	if err != nil {
		t.Fatalf("read caption: %v", err)
	}
	return caption
}

func (h *harness) captionRows(t *testing.T, assetID string) int {
	t.Helper()
	var n int
	if err := h.store.Pool().QueryRow(context.Background(),
		"select count(*) from asset_descriptions where asset_id = $1::uuid", assetID).Scan(&n); err != nil {
		t.Fatalf("count captions: %v", err)
	}
	return n
}

func (h *harness) ocr(t *testing.T, assetID string) string {
	t.Helper()
	var text string
	err := h.store.Pool().QueryRow(context.Background(),
		"select text from asset_ocr where asset_id = $1::uuid and model = $2",
		assetID, db.OCRModel).Scan(&text)
	if err != nil {
		t.Fatalf("read recognised text: %v", err)
	}
	return text
}

func (h *harness) tags(t *testing.T, assetID string) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(),
		`select t.name from asset_tags at join tags t on t.id = at.tag_id
		 where at.asset_id = $1::uuid order by t.name`, assetID)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan tag: %v", err)
		}
		names = append(names, name)
	}
	return names
}

func (h *harness) searchable(t *testing.T, assetID, phrase string) bool {
	t.Helper()
	var found bool
	err := h.store.Pool().QueryRow(context.Background(), `
		select exists (
			select 1 from asset_search
			where asset_id = $1::uuid and tsv @@ websearch_to_tsquery('english', $2))`,
		assetID, phrase).Scan(&found)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return found
}

func lengths(batches [][]string) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}
	return out
}
