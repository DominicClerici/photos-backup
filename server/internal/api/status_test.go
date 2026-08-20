package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

func decodeStatus(t *testing.T, resp *http.Response) statusResponse {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/status returned %d", resp.StatusCode)
	}
	var out statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return out
}

func TestStatusReportsTheLibraryAndTheDrive(t *testing.T) {
	h := newHarness(t)
	h.upload(t, loadFixture(t), nil)

	status := decodeStatus(t, h.get(t, "/v1/status"))

	if status.Library.Items != 1 || status.Library.Photos != 1 {
		t.Errorf("library = %+v, want one item, one photo", status.Library)
	}
	// The volume figures come from the kernel, so the only thing worth
	// asserting is that they were read at all — and that the pie can close.
	if status.Storage.Archive.Total == 0 {
		t.Error("archive volume total = 0; statfs was never read")
	}
	if got := status.Storage.Archive.Used + status.Storage.Archive.Free; got != status.Storage.Archive.Total {
		t.Errorf("used + free = %d, want %d: the storage donut would not close", got, status.Storage.Archive.Total)
	}
	if status.Storage.Photos == 0 {
		t.Error("photo bytes = 0 after an upload")
	}
}

// An upload queues a metadata job, and the queue card is how you find out the
// gallery is waiting on one.
func TestStatusCountsTheQueue(t *testing.T) {
	h := newHarness(t)
	h.upload(t, loadFixture(t), nil)

	status := decodeStatus(t, h.get(t, "/v1/status"))
	if status.Queue.Pending == 0 {
		t.Errorf("pending = 0, want the metadata job the upload queued (kinds: %+v)", status.Queue.Kinds)
	}
	if status.Queue.Failed != 0 {
		t.Errorf("failed = %d, want 0", status.Queue.Failed)
	}
}

// The point of the failures list: the error text, and enough about the asset to
// recognise which photograph it is.
func TestStatusNamesTheAssetBehindAFailedJob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	upload := decodeUpload(t, h.upload(t, loadFixture(t), nil))

	if _, err := h.store.Pool().Exec(ctx,
		`update jobs set state = 'failed', last_error = $1, attempts = 5 where asset_id = $2`,
		"exiftool: exit status 1", upload.ID); err != nil {
		t.Fatalf("fail the job: %v", err)
	}

	status := decodeStatus(t, h.get(t, "/v1/status"))
	if status.Queue.Failed != 1 {
		t.Fatalf("failed = %d, want 1", status.Queue.Failed)
	}
	if len(status.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(status.Failures))
	}

	f := status.Failures[0]
	if f.Error != "exiftool: exit status 1" {
		t.Errorf("error = %q, want the message the job gave up with", f.Error)
	}
	if f.Kind != jobs.KindMetadata {
		t.Errorf("kind = %q, want metadata", f.Kind)
	}
	if f.Filename != "IMG_8071.HEIC" || !f.Viewable {
		t.Errorf("failure = %+v, want the filename and a thumbnail to draw", f)
	}
	if f.Attempts != 5 {
		t.Errorf("attempts = %d, want 5", f.Attempts)
	}
}

// A server that will never build a thumbnail should say so on the page whose
// job is to say what is wrong, rather than leaving a library of grey tiles as
// the only symptom.
func TestStatusReportsDisabledWorkers(t *testing.T) {
	h := newHarness(t)

	if problems := decodeStatus(t, h.get(t, "/v1/status")).Problems; hasProblem(problems, "worker-disabled") {
		t.Fatal("worker-disabled reported by a server that is running its workers")
	}

	h.srv.WorkerEnabled = false
	if problems := decodeStatus(t, h.get(t, "/v1/status")).Problems; !hasProblem(problems, "worker-disabled") {
		t.Errorf("problems = %+v, want worker-disabled", problems)
	}
}

func TestStatusReportsAMissingTool(t *testing.T) {
	h := newHarness(t)
	h.srv.Tools = []Tool{{Binary: "a-binary-that-is-not-installed", Needs: "nothing at all"}}

	problems := decodeStatus(t, h.get(t, "/v1/status")).Problems
	if !hasProblem(problems, "tool-a-binary-that-is-not-installed") {
		t.Errorf("problems = %+v, want the missing tool", problems)
	}
}

func hasProblem(problems []problem, id string) bool {
	for _, p := range problems {
		if p.ID == id {
			return true
		}
	}
	return false
}
