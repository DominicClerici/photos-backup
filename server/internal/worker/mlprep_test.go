package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/derivstore"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// dimensions asks ImageMagick how big a rendition came out. The test needs the
// real numbers because the whole difference between this rendition and the
// thumbnail beside it is its shape.
func dimensions(t *testing.T, path string) (width, height int) {
	t.Helper()
	out, err := exec.Command("magick", path, "-format", "%w %h", "info:").Output()
	if err != nil {
		t.Fatalf("measure %s: %v", path, err)
	}
	if _, err := fmt.Sscan(string(out), &width, &height); err != nil {
		t.Fatalf("measure %s: %q is not a size", path, out)
	}
	return width, height
}

// The point of the rendition, in one assertion: a 3024x4032 photograph comes out
// three-quarters as wide as it is tall, and not square. A square is a crop, and
// a crop is a machine deciding in advance what the photograph was about.
//
// Written against derivstore.MLEdge rather than the number it happens to be, so
// that changing the rendition size stays a one-constant change. It was 512 and
// is 1536; what must not change is the aspect ratio.
func TestMLPrepRendersTheWholeFrameUncropped(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	path := h.Derivatives.Path(asset.SHA256, derivstore.MLStill)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the ML rendition was not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the ML rendition is empty")
	}

	w, hgt := dimensions(t, path)
	if hgt != derivstore.MLEdge {
		t.Errorf("height = %d, want %d on the longest edge", hgt, derivstore.MLEdge)
	}
	if want := derivstore.MLEdge * 3 / 4; w != want {
		t.Errorf("width = %d, want %d — a 3024x4032 photograph fitted to %d on its longest edge",
			w, want, derivstore.MLEdge)
	}
	if w == hgt {
		t.Error("the rendition is square, so it was cropped")
	}
}

// The metadata job is what queues this, at the same moment and with the same
// exclusions as the signature.
func TestTheMetadataJobQueuesTheMLRenditions(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	if state := h.jobState(t, asset.ID, jobs.KindMLPrep); state != string(jobs.StatePending) {
		t.Errorf("mlprep job state = %q, want pending", state)
	}
}

func TestMLPrepSamplesAVideoAcrossItsLength(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "clip.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	frames := 0
	for i := range derivstore.MLFrameCount {
		info, err := os.Stat(h.Derivatives.Path(asset.SHA256, derivstore.MLFrameSuffix(i)))
		if err != nil {
			break
		}
		if info.Size() == 0 {
			t.Fatalf("frame %d is empty", i)
		}
		frames++
	}
	if frames == 0 {
		t.Fatal("a video was sampled into no frames at all")
	}

	// The still rendition is not written for a video: the frames are its
	// renditions, and a seventh file would be a picture of the poster.
	if _, err := os.Stat(h.Derivatives.Path(asset.SHA256, derivstore.MLStill)); err == nil {
		t.Error("a video also got a still ML rendition")
	}

	// Uncropped applies here too. The fixture is landscape, so its frames must
	// be wider than they are tall.
	w, hgt := dimensions(t, h.Derivatives.Path(asset.SHA256, derivstore.MLFrameSuffix(0)))
	if w <= hgt {
		t.Errorf("frame 0 is %dx%d; a landscape clip was squeezed or cropped", w, hgt)
	}
}

// A container ffmpeg cannot decode costs the search feature one video, and
// nothing else. Parking the job would mark a perfectly good archived original
// as broken over a question nobody has asked.
func TestAVideoThatCannotBeSampledIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	asset := h.ingest(t, "undecodable.mov", db.MediaVideo)

	h.claimAndRun(t, jobs.KindMetadata)
	job := h.claimAndRun(t, jobs.KindMLPrep)

	if state := h.jobState(t, asset.ID, jobs.KindMLPrep); state != string(jobs.StateDone) {
		t.Errorf("mlprep job %d state = %q, want done", job.ID, state)
	}
}

// The four exclusions, checked where they are cheapest to check: the reconcile
// that would otherwise queue the whole archive.
func TestTheReconcileSkipsEverythingThatIsNotAPhotograph(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	shown := h.ingest(t, "iphone-portrait.heic", db.MediaImage)
	overlay := h.ingest(t, "photo.jpg", db.MediaImage)
	trashed := h.ingest(t, "sample.heic", db.MediaImage)
	pair := h.ingest(t, "live-clip.mov", db.MediaVideo)

	pool := h.store.Pool()
	if _, err := pool.Exec(ctx, `update assets set is_overlay = true where id = $1::uuid`, overlay.ID); err != nil {
		t.Fatalf("mark an overlay: %v", err)
	}
	if _, err := pool.Exec(ctx, `update assets set deleted_at = now() where id = $1::uuid`, trashed.ID); err != nil {
		t.Fatalf("trash an asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`update assets set live_parent_local_id = 'somebodys-still' where id = $1::uuid`, pair.ID); err != nil {
		t.Fatalf("mark a paired video: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from jobs where kind = 'mlprep'`); err != nil {
		t.Fatalf("clear queued renditions: %v", err)
	}

	n, err := jobs.ReconcileMLPrep(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcileMLPrep: %v", err)
	}
	if n != 1 {
		t.Fatalf("queued %d assets, want just the one the timeline shows", n)
	}

	var queued string
	if err := pool.QueryRow(ctx,
		`select asset_id::text from jobs where kind = 'mlprep'`).Scan(&queued); err != nil {
		t.Fatalf("read the queued job: %v", err)
	}
	if queued != shown.ID {
		t.Errorf("queued %s, want %s", queued, shown.ID)
	}

	// And a second start over the same library queues nothing, because the
	// evidence this ran is the job row.
	again, err := jobs.ReconcileMLPrep(ctx, pool)
	if err != nil {
		t.Fatalf("ReconcileMLPrep again: %v", err)
	}
	if again != 0 {
		t.Errorf("a restart queued %d assets that already had renditions", again)
	}
}

// Every rendition an asset can have has to be reachable by the purge and by the
// vault, both of which work from this one list.
func TestTheMLRenditionsAreInTheSuffixContract(t *testing.T) {
	want := []string{derivstore.MLStill}
	for i := range derivstore.MLFrameCount {
		want = append(want, derivstore.MLFrameSuffix(i))
	}

	have := strings.Join(derivstore.Suffixes(), " ")
	for _, suffix := range want {
		if !strings.Contains(have, suffix+" ") && !strings.HasSuffix(have, suffix) {
			t.Errorf("%s is not in derivstore.Suffixes(), so a purge would leave it behind", suffix)
		}
	}
}

// The regression that migration 0021 is about: `ml renditions` re-renders the
// archive and must queue nothing that reads the renditions.
//
// The first mlprep is an arrival and queues all three passes, which is the
// feature. The second is RequeueMLPrep's, and the assertion that matters is on
// describe: an hour of CPU must not turn into four hours of GPU, and it did,
// because most of the archive had no describe row for do-nothing-on-conflict to
// protect.
func TestReRenderingQueuesNoWorkThatReadsTheRenditions(t *testing.T) {
	h := newHarness(t).withML(newFakeML(t).URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)
	h.claimAndRun(t, jobs.KindMLPrep)

	for _, kind := range []jobs.Kind{jobs.KindVision, jobs.KindOCR, jobs.KindDescribe} {
		if state := h.jobStateOrNone(t, asset.ID, kind); state != string(jobs.StatePending) {
			t.Fatalf("after the arrival, %s job state = %q, want pending", kind, state)
		}
	}

	// Drain them, so that what the re-render does next is visible: a row left
	// pending would be indistinguishable from one this requeued.
	for _, kind := range []jobs.Kind{jobs.KindVision, jobs.KindOCR, jobs.KindDescribe} {
		h.claimAndRun(t, kind)
	}

	if _, err := jobs.RequeueMLPrep(context.Background(), h.store.Pool()); err != nil {
		t.Fatalf("requeue mlprep: %v", err)
	}
	h.claimAndRun(t, jobs.KindMLPrep)

	for _, kind := range []jobs.Kind{jobs.KindVision, jobs.KindOCR, jobs.KindDescribe} {
		if state := h.jobStateOrNone(t, asset.ID, kind); state != string(jobs.StateDone) {
			t.Errorf("after the re-render, %s job state = %q, want it left done", kind, state)
		}
	}
}

// The other half of the same bit: an asset that has never been through any ML
// pass gets no describe job out of a re-render either. This is the case that
// actually bit, and the one do-nothing-on-conflict could not catch — there was
// no row to conflict with.
func TestReRenderingQueuesNothingForAnAssetThatHasNeverBeenDescribed(t *testing.T) {
	h := newHarness(t).withML(newFakeML(t).URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)

	h.claimAndRun(t, jobs.KindMetadata)

	// Take the arrival's mlprep out of the way unrun, leaving an asset with
	// renditions owed and no ML rows at all — which is every asset the bounded
	// captioning backfill never reached.
	if _, err := h.store.Pool().Exec(context.Background(),
		`delete from jobs where asset_id = $1::uuid and kind = 'mlprep'`, asset.ID); err != nil {
		t.Fatalf("clear the arrival's mlprep: %v", err)
	}

	if _, err := jobs.RequeueMLPrep(context.Background(), h.store.Pool()); err != nil {
		t.Fatalf("requeue mlprep: %v", err)
	}
	h.claimAndRun(t, jobs.KindMLPrep)

	if state := h.jobStateOrNone(t, asset.ID, jobs.KindDescribe); state != "" {
		t.Errorf("the re-render queued a describe job (state %q) for an asset that had none", state)
	}
	if state := h.jobStateOrNone(t, asset.ID, jobs.KindOCR); state != "" {
		t.Errorf("the re-render queued an ocr job (state %q) for an asset that had none", state)
	}
}

// A re-render is hours of CPU and somebody will interrupt it. Typing the command
// again has to reach the rows the first run left pending, or most of the archive
// keeps the intent it was queued with — which on the run that prompted all this
// would have been 11,555 of 17,916 assets still queueing a captioner.
func TestReRenderingAdoptsTheRowsAnInterruptedOneLeftPending(t *testing.T) {
	h := newHarness(t).withML(newFakeML(t).URL)
	asset := h.ingest(t, "iphone-portrait.heic", db.MediaImage)
	ctx := context.Background()

	h.claimAndRun(t, jobs.KindMetadata)

	// The arrival's mlprep, still owed: this is the state an interrupted
	// re-render leaves behind, and the state the old conflict guard could not
	// reach.
	if state := h.jobStateOrNone(t, asset.ID, jobs.KindMLPrep); state != string(jobs.StatePending) {
		t.Fatalf("mlprep job state = %q, want pending", state)
	}

	if _, err := jobs.RequeueMLPrep(ctx, h.store.Pool()); err != nil {
		t.Fatalf("requeue mlprep: %v", err)
	}
	h.claimAndRun(t, jobs.KindMLPrep)

	if state := h.jobStateOrNone(t, asset.ID, jobs.KindDescribe); state != "" {
		t.Errorf("the re-render queued a describe job (state %q) for a row it found pending", state)
	}
}
