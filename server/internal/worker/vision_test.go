package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// fakeML is photo-ml's protocol without the GPU.
//
// Stubbed here where nothing else in this package is, and for the reason the
// rest is not: everywhere else this worker sequences real tools, so a test with
// a stubbed ffmpeg would verify the stub. What is under test here is not what a
// model says — it is what this worker does with an answer, with a refusal, and
// with silence, and the last two are states a real service will not hold on
// request.
type fakeML struct {
	*httptest.Server
	ready bool
	// requests counts the images actually sent, which is how the tests below
	// assert that a video arrives as six pictures rather than one.
	requests [][]string
	// described and recognised are the same record for the two routes that
	// read pictures rather than compare them, kept apart so a test can say
	// which model was handed what.
	described  [][]string
	recognised [][]string
	// mayRelease records, per ocr request, whether photod said it had no
	// captioning work outstanding — the fact that decides whether the recogniser
	// may take the card back from an idle captioner. See residency.release.
	mayRelease []bool
	// captions and text are what the fake answers with, per image index, so a
	// test can assert that three frames of a clip come back as one asset's
	// worth of words.
	captions []string
	text     []string
}

func newFakeML(t *testing.T) *fakeML {
	t.Helper()
	f := &fakeML{ready: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "ready": f.ready, "device": "cpu", "dtype": "float32",
			"models": []map[string]any{{
				"key": "vision", "model": db.VisionModel, "role": "resident", "resident": f.ready,
			}},
		})
	})
	mux.HandleFunc("POST /embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Images []string `json:"images"`
			Texts  []string `json:"texts"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.requests = append(f.requests, req.Images)

		vectors := make([][]float32, len(req.Images)+len(req.Texts))
		for i := range vectors {
			v := make([]float32, db.VisionDim)
			v[i%db.VisionDim] = 1
			vectors[i] = v
		}
		json.NewEncoder(w).Encode(map[string]any{
			"model": db.VisionModel, "dim": db.VisionDim, "normalized": true, "vectors": vectors,
		})
	})
	mux.HandleFunc("POST /describe", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Images []string `json:"images"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.described = append(f.described, req.Images)

		results := make([]map[string]any, len(req.Images))
		for i := range results {
			caption := "a photograph"
			if i < len(f.captions) {
				caption = f.captions[i]
			}
			results[i] = map[string]any{
				"caption": caption,
				"tags": []map[string]any{
					{"name": "dog", "confidence": 0.9},
					{"name": "beach", "confidence": 0.8},
				},
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"model": db.CaptionModel, "results": results,
		})
	})
	mux.HandleFunc("POST /ocr", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Images             []string `json:"images"`
			DescribeQueueEmpty bool     `json:"describe_queue_empty"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.recognised = append(f.recognised, req.Images)
		f.mayRelease = append(f.mayRelease, req.DescribeQueueEmpty)

		results := make([]map[string]any, len(req.Images))
		for i := range results {
			text := ""
			if i < len(f.text) {
				text = f.text[i]
			}
			results[i] = map[string]any{"text": text, "lines": []any{}}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"model": db.OCRModel, "results": results,
		})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// withML points a harness at a photo-ml. Called before the mlprep job runs when
// a test wants the vision work queued the way an upload would queue it.
func (h *harness) withML(url string) *harness {
	h.ML = mlclient.New(url)
	return h
}

func (h *harness) embeddings(t *testing.T, assetID string) []int {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(),
		"select frame from asset_embeddings where asset_id = $1::uuid and model = $2 order by frame",
		assetID, db.VisionModel)
	if err != nil {
		t.Fatalf("read embeddings: %v", err)
	}
	defer rows.Close()

	var frames []int
	for rows.Next() {
		var frame int
		if err := rows.Scan(&frame); err != nil {
			t.Fatalf("scan frame: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// The whole path, end to end: an original arrives, Go decodes it into a
// rendition, the rendition goes out over HTTP, and 1152 numbers come back and
// land on the row.
func TestVisionEmbedsTheRenditionAPhotographProduced(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindVision)

	if got := h.embeddings(t, asset.ID); len(got) != 1 || got[0] != 0 {
		t.Fatalf("frames = %v, want a single frame 0 for a still", got)
	}
	if len(ml.requests) != 1 || len(ml.requests[0]) != 1 {
		t.Fatalf("sent %v, want one image in one request", ml.requests)
	}
	if state := h.jobState(t, asset.ID, jobs.KindVision); state != string(jobs.StateDone) {
		t.Errorf("vision job state = %q, want done", state)
	}
}

// A clip is not one picture. Six frames go out in one call and six rows come
// back, which is what lets a video that starts on a beach and ends in a
// restaurant be found as both.
func TestVisionEmbedsEveryFrameOfAVideo(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	h.claimAndRun(t, jobs.KindVision)

	frames := h.embeddings(t, asset.ID)
	if len(frames) != derivstore.MLFrameCount {
		t.Fatalf("frames = %v, want %d", frames, derivstore.MLFrameCount)
	}
	for i, frame := range frames {
		if frame != i {
			t.Fatalf("frames = %v, want them numbered from zero", frames)
		}
	}
	// One request, not six. The batch is the reason a backfill is fifteen
	// minutes rather than an hour of round trips.
	if len(ml.requests) != 1 || len(ml.requests[0]) != derivstore.MLFrameCount {
		t.Fatalf("sent %d requests %v, want all six frames together", len(ml.requests), ml.requests)
	}
}

// The mlprep job queues this, at the moment the file it needs exists.
func TestMLPrepQueuesTheEmbeddingPass(t *testing.T) {
	h := newHarness(t).withML(newFakeML(t).URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	if state := h.jobStateOrNone(t, asset.ID, jobs.KindVision); state != "" {
		t.Fatalf("vision job state = %q before the renditions exist, want no job at all", state)
	}

	h.claimAndRun(t, jobs.KindMLPrep)
	if state := h.jobState(t, asset.ID, jobs.KindVision); state != string(jobs.StatePending) {
		t.Errorf("vision job state = %q, want pending once the renditions are on disk", state)
	}
}

// A machine with no GPU service does not grow a backlog describing a feature it
// does not have. Setting ML_URL and restarting is what turns the library into
// queued work, in one place.
func TestNoPhotoMLQueuesNoVisionWork(t *testing.T) {
	h := newHarness(t) // no withML
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	if state := h.jobStateOrNone(t, asset.ID, jobs.KindVision); state != "" {
		t.Errorf("vision job state = %q with no photo-ml configured, want no job at all", state)
	}
}

// The failure mode this whole design is arranged around. photo-ml going away
// mid-backfill must cost the queue nothing: the job goes back with its attempt
// returned, not five swings at a closed socket and a permanently failed
// library.
func TestPhotoMLGoingAwayCostsTheQueueNothing(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	ml.Close() // systemctl restart photo-ml, from the queue's point of view
	job := h.claimAndRun(t, jobs.KindVision)

	state, attempts := h.jobStateAndAttempts(t, job.ID)
	if state != string(jobs.StatePending) {
		t.Fatalf("state = %q, want pending — the work was never attempted", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0: the job never reached the bytes", attempts)
	}
	if got := h.embeddings(t, asset.ID); len(got) != 0 {
		t.Errorf("stored %v, want nothing", got)
	}
}

// An embedding is a searchable description of what a photograph looks like,
// which is the vault's whole objection. The renditions were sealed on the way
// in, so there is nothing to send either — this is the guard that makes the
// second fact deliberate rather than incidental.
func TestVisionSkipsAVaultedAsset(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	if _, err := h.store.Pool().Exec(context.Background(),
		"update assets set vault = 'hidden' where id = $1::uuid", asset.ID); err != nil {
		t.Fatalf("hide: %v", err)
	}

	h.claimAndRun(t, jobs.KindVision)

	if len(ml.requests) != 0 {
		t.Errorf("sent %v to photo-ml for a hidden photograph, want nothing", ml.requests)
	}
	if state := h.jobState(t, asset.ID, jobs.KindVision); state != string(jobs.StateDone) {
		t.Errorf("state = %q, want done: there is genuinely nothing left to do", state)
	}
}

// The tolerance clipRenditions applies to an unsampleable video has to reach
// this end of the pipe too, or the leniency upstream just moves the failure one
// job to the right and marks a perfectly good archived clip broken.
func TestNoRenditionsIsNotAFailure(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)
	if err := h.Derivatives.Remove(asset.SHA256, derivstore.MLStill); err != nil {
		t.Fatalf("remove the rendition: %v", err)
	}

	h.claimAndRun(t, jobs.KindVision)

	if state := h.jobState(t, asset.ID, jobs.KindVision); state != string(jobs.StateDone) {
		t.Errorf("state = %q, want done", state)
	}
	if len(ml.requests) != 0 {
		t.Errorf("sent %v with nothing on disk, want no call at all", ml.requests)
	}
}

// The gate, which is why the pool does not need an error path for "not
// installed": it asks before it claims. `ready` rather than `ok`, because
// photo-ml answers /health from its first moment on purpose and handing work to
// a service still pulling weights would only produce a timeout.
func TestTheVisionPoolWaitsForAWarmService(t *testing.T) {
	ml := newFakeML(t)
	h := newHarness(t).withML(ml.URL)
	ctx := context.Background()

	if !h.mlAvailable(ctx) {
		t.Fatal("a warm service should be available")
	}

	ml.ready = false
	h.mlCheckedAt = h.mlCheckedAt.Add(-mlProbeInterval) // expire the cache
	if h.mlAvailable(ctx) {
		t.Error("a service that is listening but still loading is not ready for work")
	}

	ml.Close()
	h.mlCheckedAt = h.mlCheckedAt.Add(-mlProbeInterval)
	if h.mlAvailable(ctx) {
		t.Error("a closed socket is not available")
	}
}

func TestNoPhotoMLIsNeverAvailable(t *testing.T) {
	if newHarness(t).mlAvailable(context.Background()) {
		t.Error("a runner with no photo-ml configured has nothing to be available")
	}
}
