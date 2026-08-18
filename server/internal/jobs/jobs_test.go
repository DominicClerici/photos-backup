package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/jobs"
)

func TestRecordAssetEnqueuesItsMetadataJob(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()

	assetID := newAsset(t, store)

	job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job.AssetID != assetID {
		t.Errorf("claimed job for %s, want %s", job.AssetID, assetID)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 after the first claim", job.Attempts)
	}
}

func TestClaimReportsAnEmptyQueue(t *testing.T) {
	q, store := testQueue(t)
	clearJobs(t, store)

	_, err := q.Claim(context.Background(), []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("claim on empty queue: got %v, want ErrNoJob", err)
	}
}

// This is the test the whole queue design rests on. SKIP LOCKED is what lets
// concurrent claimers share one queue with no coordinator; if it were wrong,
// two workers would run ffmpeg on the same asset simultaneously.
func TestConcurrentClaimersNeverGetTheSameJob(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	clearJobs(t, store)

	const jobCount = 24
	const claimers = 8

	want := make(map[string]bool, jobCount)
	for range jobCount {
		id := newAsset(t, store)
		want[id] = true
	}
	// newAsset enqueued a metadata job per asset, which is exactly the fixture.

	var (
		mu     sync.Mutex
		claims []string
		wg     sync.WaitGroup
	)
	for c := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, fmt.Sprintf("worker-%d", c))
				if errors.Is(err, jobs.ErrNoJob) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				claims = append(claims, job.AssetID)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claims) != jobCount {
		t.Fatalf("claimed %d jobs, want %d", len(claims), jobCount)
	}
	seen := make(map[string]bool, len(claims))
	for _, id := range claims {
		if seen[id] {
			t.Errorf("asset %s was claimed twice", id)
		}
		seen[id] = true
		if !want[id] {
			t.Errorf("claimed an asset that was never queued: %s", id)
		}
	}
}

func TestClaimOnlyTakesRequestedKinds(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()

	assetID := newAsset(t, store)
	if err := jobs.Enqueue(ctx, store.Pool(), jobs.KindPlayback, assetID); err != nil {
		t.Fatalf("enqueue playback: %v", err)
	}

	job, err := q.Claim(ctx, []jobs.Kind{jobs.KindPlayback}, "transcoder")
	if err != nil {
		t.Fatalf("claim playback: %v", err)
	}
	if job.Kind != jobs.KindPlayback {
		t.Fatalf("claimed kind %q, want playback", job.Kind)
	}

	// The metadata job for the same asset must still be sitting there. This is
	// the property that keeps a slow transcode pool from consuming thumbnails.
	next, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if err != nil {
		t.Fatalf("claim metadata: %v", err)
	}
	if next.Kind != jobs.KindMetadata {
		t.Errorf("claimed kind %q, want metadata", next.Kind)
	}
}

func TestFailReschedulesUntilAttemptsRunOut(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	newAsset(t, store)

	// No backoff, so the retries are immediately claimable and the test does not
	// have to wait out an exponential curve.
	q.BaseBackoff = time.Nanosecond
	q.MaxBackoff = time.Nanosecond
	q.MaxAttempts = 3

	boom := errors.New("magick: no decode delegate for this image format")

	for attempt := 1; attempt <= q.MaxAttempts; attempt++ {
		job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
		if err != nil {
			t.Fatalf("claim on attempt %d: %v", attempt, err)
		}
		if job.Attempts != attempt {
			t.Fatalf("attempts = %d on claim %d", job.Attempts, attempt)
		}

		permanent, err := q.Fail(ctx, job, boom)
		if err != nil {
			t.Fatalf("fail on attempt %d: %v", attempt, err)
		}
		wantPermanent := attempt == q.MaxAttempts
		if permanent != wantPermanent {
			t.Fatalf("attempt %d reported permanent=%v, want %v", attempt, permanent, wantPermanent)
		}
	}

	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1"); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("a failed job was claimed again: %v", err)
	}

	failed, err := q.Failed(ctx, 10)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed jobs = %d, want 1", len(failed))
	}
	if failed[0].Error != boom.Error() {
		t.Errorf("stored error = %q, want the cause verbatim", failed[0].Error)
	}
}

func TestFailedJobIsNotClaimableBeforeItsBackoffElapses(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	newAsset(t, store)

	q.BaseBackoff = time.Hour
	q.MaxBackoff = time.Hour

	job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := q.Fail(ctx, job, errors.New("transient")); err != nil {
		t.Fatalf("fail: %v", err)
	}

	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1"); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("claimed a job still in backoff: %v", err)
	}
}

func TestReclaimReturnsAbandonedWorkToTheQueue(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	newAsset(t, store)

	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "doomed-worker"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The worker dies here without completing or failing the job.

	// A zero lease makes every running job look expired, which is what a
	// process that was killed mid-job leaves behind.
	q.Lease = time.Nanosecond
	n, err := q.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d jobs, want 1", n)
	}

	job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-2")
	if err != nil {
		t.Fatalf("claim after reclaim: %v", err)
	}
	if job.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 — the abandoned attempt still counts", job.Attempts)
	}
}

// A file that reliably kills its worker must not cycle forever: the attempt
// budget applies to abandoned jobs too, not just ones that reported failure.
func TestReclaimGivesUpOnAJobOutOfAttempts(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	newAsset(t, store)

	q.Lease = time.Nanosecond
	q.MaxAttempts = 2

	for range q.MaxAttempts {
		if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "doomed-worker"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := q.ReclaimExpired(ctx); err != nil {
			t.Fatalf("reclaim: %v", err)
		}
	}

	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1"); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("a job past its attempt budget was claimed again: %v", err)
	}
	failed, err := q.Failed(ctx, 10)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed jobs = %d, want 1", len(failed))
	}
}

func TestCompleteTakesTheJobOutOfRotation(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	newAsset(t, store)

	job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := q.Complete(ctx, job.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1"); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("a completed job was claimed again: %v", err)
	}

	counts, err := q.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if len(counts) != 1 || counts[0].State != jobs.StateDone || counts[0].Count != 1 {
		t.Errorf("counts = %+v, want one done metadata job", counts)
	}
}

func TestEnqueueIgnoresWorkAlreadyQueued(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	assetID := newAsset(t, store)

	// RecordAsset already queued this kind; a second enqueue must not duplicate it.
	if err := jobs.Enqueue(ctx, store.Pool(), jobs.KindMetadata, assetID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1"); !errors.Is(err, jobs.ErrNoJob) {
		t.Fatalf("enqueue created a duplicate job: %v", err)
	}
}

func TestRequeueRunsAFinishedJobAgain(t *testing.T) {
	q, store := testQueue(t)
	ctx := context.Background()
	assetID := newAsset(t, store)

	job, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := q.Complete(ctx, job.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if err := jobs.Requeue(ctx, store.Pool(), jobs.KindMetadata, assetID); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	again, err := q.Claim(ctx, []jobs.Kind{jobs.KindMetadata}, "worker-1")
	if err != nil {
		t.Fatalf("claim after requeue: %v", err)
	}
	if again.Attempts != 1 {
		t.Errorf("attempts = %d after requeue, want the counter reset to 1", again.Attempts)
	}
}
