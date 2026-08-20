package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/db"
	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

// The reason Defer exists, stated as a test: an outage must cost nothing.
//
// Without it, restarting photo-ml during a backfill takes five swings at a
// closed socket for every queued asset and parks the library as permanently
// failed, and the way back is a hand-written UPDATE over sixty thousand rows.
func TestDeferReturnsTheAttempt(t *testing.T) {
	queue, store := testQueue(t)
	ctx := context.Background()
	id := newAsset(t, store)
	clearJobs(t, store)

	if err := jobs.Enqueue(ctx, store.Pool(), jobs.KindVision, id); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := queue.Claim(ctx, []jobs.Kind{jobs.KindVision}, "vision-0")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job.Attempts != 1 {
		t.Fatalf("attempts = %d on a first claim, want 1", job.Attempts)
	}

	if err := queue.Defer(ctx, job, time.Millisecond, "photo-ml is unavailable"); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	state, attempts, lastErr := jobRow(t, store, job.ID)
	if state != string(jobs.StatePending) {
		t.Fatalf("state = %q, want pending", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d after a deferral, want 0 — the job never reached the bytes", attempts)
	}
	// Kept, because the status page showing "waiting on photo-ml" is the
	// difference between a stalled queue and a mysterious one.
	if lastErr != "photo-ml is unavailable" {
		t.Fatalf("last_error = %q, want the reason preserved", lastErr)
	}

	// And it is claimable again, by the same pool, once the delay is up.
	time.Sleep(20 * time.Millisecond)
	if _, err := queue.Claim(ctx, []jobs.Kind{jobs.KindVision}, "vision-0"); err != nil {
		t.Fatalf("re-claim after deferral: %v", err)
	}
}

// A job the lease sweep already took back belongs to the queue, and possibly to
// another worker. Writing a stale run_after onto it would move work somebody
// else is holding.
func TestDeferLeavesAJobItNoLongerHolds(t *testing.T) {
	queue, store := testQueue(t)
	ctx := context.Background()
	id := newAsset(t, store)
	clearJobs(t, store)

	if err := jobs.Enqueue(ctx, store.Pool(), jobs.KindVision, id); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := queue.Claim(ctx, []jobs.Kind{jobs.KindVision}, "vision-0")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, "update jobs set state = 'pending', attempts = 3 where id = $1", job.ID); err != nil {
		t.Fatalf("simulate a reclaim: %v", err)
	}

	if err := queue.Defer(ctx, job, time.Hour, "too late"); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if _, attempts, lastErr := jobRow(t, store, job.ID); attempts != 3 || lastErr != "" {
		t.Fatalf("attempts=%d last_error=%q, want the row left exactly as the sweep left it", attempts, lastErr)
	}
}

// The backfill, and the model swap, are the same query. Both hinge on mlprep
// being done — which is the dependency and the timeline filter at once, since
// the exclusions that decide who gets a rendition are already baked into that
// row rather than restated here.
func TestReconcileVisionFollowsTheRenditions(t *testing.T) {
	queue, store := testQueue(t)
	ctx := context.Background()
	_ = queue

	ready := newAsset(t, store)
	notPrepped := newAsset(t, store)
	clearJobs(t, store)
	markPrepDone(t, store, ready)

	n, err := jobs.ReconcileVision(ctx, store.Pool(), db.VisionModel)
	if err != nil {
		t.Fatalf("ReconcileVision: %v", err)
	}
	if n != 1 {
		t.Fatalf("queued %d, want only the asset whose renditions exist", n)
	}
	if !hasJob(t, store, ready, jobs.KindVision) {
		t.Error("the prepped asset should have vision work queued")
	}
	if hasJob(t, store, notPrepped, jobs.KindVision) {
		t.Error("an asset with no renditions has nothing for photo-ml to look at")
	}

	// Idempotent while the answer has not changed.
	if again, err := jobs.ReconcileVision(ctx, store.Pool(), db.VisionModel); err != nil || again != 0 {
		t.Fatalf("second run queued %d (err %v), want 0", again, err)
	}
}

func TestReconcileVisionSkipsWhatThisModelHasAlreadyDescribed(t *testing.T) {
	_, store := testQueue(t)
	ctx := context.Background()
	id := newAsset(t, store)
	clearJobs(t, store)
	markPrepDone(t, store, id)

	if err := store.PutEmbeddings(ctx, id, db.VisionModel, []db.Embedding{{Vector: make([]float32, db.VisionDim)}}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}
	if n, err := jobs.ReconcileVision(ctx, store.Pool(), db.VisionModel); err != nil || n != 0 {
		t.Fatalf("queued %d (err %v) for an asset this model has already described, want 0", n, err)
	}

	// A different model has said nothing about it, so a bench pass finds it —
	// which is the property that makes swapping encoders a delete and a
	// restart rather than a migration.
	if n, err := jobs.ReconcileVision(ctx, store.Pool(), "some-other-encoder"); err != nil || n != 1 {
		t.Fatalf("queued %d (err %v) for a model with no vectors, want 1", n, err)
	}
}

// The swap itself: drop the old model's vectors, restart, and the done rows go
// back to pending rather than staying done and leaving the library undescribed.
func TestReconcileVisionRequeuesAfterTheVectorsAreDropped(t *testing.T) {
	_, store := testQueue(t)
	ctx := context.Background()
	id := newAsset(t, store)
	clearJobs(t, store)
	markPrepDone(t, store, id)

	if err := store.PutEmbeddings(ctx, id, db.VisionModel, []db.Embedding{{Vector: make([]float32, db.VisionDim)}}); err != nil {
		t.Fatalf("PutEmbeddings: %v", err)
	}
	if _, err := store.Pool().Exec(ctx,
		`insert into jobs (kind, asset_id, state) values ('vision', $1::uuid, 'done')`, id); err != nil {
		t.Fatalf("record the finished pass: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, "delete from asset_embeddings where model = $1", db.VisionModel); err != nil {
		t.Fatalf("drop the old model's vectors: %v", err)
	}

	if n, err := jobs.ReconcileVision(ctx, store.Pool(), db.VisionModel); err != nil || n != 1 {
		t.Fatalf("requeued %d (err %v), want the done job back in the queue", n, err)
	}
	if state, _, _ := jobRowFor(t, store, id, jobs.KindVision); state != string(jobs.StatePending) {
		t.Fatalf("state = %q, want pending", state)
	}
}

func TestReconcileVisionSkipsTheVaultAndTheTrash(t *testing.T) {
	_, store := testQueue(t)
	ctx := context.Background()
	hidden := newAsset(t, store)
	trashed := newAsset(t, store)
	clearJobs(t, store)
	markPrepDone(t, store, hidden)
	markPrepDone(t, store, trashed)

	if _, err := store.Pool().Exec(ctx, "update assets set vault = 'hidden' where id = $1::uuid", hidden); err != nil {
		t.Fatalf("hide: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, "update assets set deleted_at = now() where id = $1::uuid", trashed); err != nil {
		t.Fatalf("trash: %v", err)
	}

	if n, err := jobs.ReconcileVision(ctx, store.Pool(), db.VisionModel); err != nil || n != 0 {
		t.Fatalf("queued %d (err %v), want nothing: one is hidden and one is deleted", n, err)
	}
}

func markPrepDone(t *testing.T, store *db.Store, assetID string) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(),
		`insert into jobs (kind, asset_id, state) values ('mlprep', $1::uuid, 'done')
		 on conflict (asset_id, kind) do update set state = 'done'`, assetID); err != nil {
		t.Fatalf("mark mlprep done: %v", err)
	}
}

func hasJob(t *testing.T, store *db.Store, assetID string, kind jobs.Kind) bool {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(),
		"select count(*) from jobs where asset_id = $1::uuid and kind = $2", assetID, string(kind)).Scan(&n); err != nil {
		t.Fatalf("look up job: %v", err)
	}
	return n > 0
}

func jobRow(t *testing.T, store *db.Store, id int64) (state string, attempts int, lastErr string) {
	t.Helper()
	if err := store.Pool().QueryRow(context.Background(),
		"select state, attempts, coalesce(last_error, '') from jobs where id = $1", id).
		Scan(&state, &attempts, &lastErr); err != nil {
		t.Fatalf("read job %d: %v", id, err)
	}
	return state, attempts, lastErr
}

func jobRowFor(t *testing.T, store *db.Store, assetID string, kind jobs.Kind) (string, int, string) {
	t.Helper()
	var id int64
	if err := store.Pool().QueryRow(context.Background(),
		"select id from jobs where asset_id = $1::uuid and kind = $2", assetID, string(kind)).Scan(&id); err != nil {
		t.Fatalf("look up %s job: %v", kind, err)
	}
	return jobRow(t, store, id)
}
