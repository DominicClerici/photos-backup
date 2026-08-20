// Package jobs is the background work queue. It owns the `jobs` table and
// nothing else: it has no idea what a thumbnail is, and the worker that runs
// the work has no idea how claiming is implemented.
//
// Claiming is a single UPDATE over a FOR UPDATE SKIP LOCKED subquery, which is
// what lets any number of concurrent claimers share one queue with no
// coordinator between them. Today those claimers are goroutines; moving them
// into a separate photo-worker process later changes nothing here.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Kind string

const (
	// KindMetadata is exiftool plus the stored thumbnails, and a poster frame for
	// video. The gallery cannot render an asset until this finishes.
	KindMetadata Kind = "metadata"
	// KindPlayback is the H.264 MP4 rendition of a video. Slow, and only the
	// viewer needs it.
	KindPlayback Kind = "playback"
	// KindSignature is what the asset looks like, in the handful of integers
	// the duplicate scan compares. See internal/imagehash.
	//
	// Its own kind, and its own pool in the worker, because it is a full decode
	// of every original in the archive and nobody is waiting for it. Behind the
	// same queue as the thumbnails it would stall the gallery for an hour to
	// answer a question that has not been asked yet.
	KindSignature Kind = "signature"
	// KindMLPrep writes the renditions the vision service reads: the whole
	// photograph, uncropped, at 512px, and several frames out of a video. It is
	// the Go half of ML_IMAGES.md §3 — Go decodes, Python does tensors, and
	// photo-ml never opens a file under /mnt/photos.
	//
	// Its own kind rather than part of a later `vision` job, so that swapping
	// the model requeues only the model's half: the renditions are already on
	// disk and decoding them again is the expensive part. Its own kind rather
	// than part of `metadata`, so that one new file per asset does not re-run
	// exiftool and three thumbnail sizes over 23,000 of them — migration 0007
	// did exactly that, correctly, for a change that genuinely needed it. This
	// one does not.
	KindMLPrep Kind = "mlprep"
	// KindMerge concatenates a set of Snapchat segments into one archived
	// recording. ffmpeg, so it runs in the transcode pool; the asset it names is
	// the first piece, which is as close as this table gets to naming a group.
	KindMerge Kind = "merge"
)

type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Defaults chosen so a transient failure (a busy disk, a killed subprocess)
// retries quickly, while a genuinely broken file gives up within the hour
// rather than burning ffmpeg cycles forever.
const (
	DefaultMaxAttempts = 5
	DefaultBaseBackoff = 30 * time.Second
	DefaultMaxBackoff  = 30 * time.Minute
	// DefaultLease is deliberately longer than any plausible transcode. Its only
	// job is to notice a worker that died, and reclaiming a job that is actually
	// still running would mean two ffmpegs writing the same output.
	DefaultLease = 10 * time.Minute
)

// Job is one unit of claimed work. Attempts is already incremented to include
// this attempt.
type Job struct {
	ID       int64
	Kind     Kind
	AssetID  string
	Attempts int
}

// Execer is the slice of pgx that both a pool and a transaction satisfy. It
// exists so Enqueue can be called inside someone else's transaction — which is
// how an asset row and its metadata job are committed together, leaving no
// window where an asset exists with no work queued for it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue adds work for an asset, ignoring a kind already queued for it. The
// conflict clause makes this safe to call from a startup reconcile or a retry
// path without checking first.
func Enqueue(ctx context.Context, q Execer, kind Kind, assetID string) error {
	const insert = `
		insert into jobs (kind, asset_id)
		values ($1, $2::uuid)
		on conflict (asset_id, kind) do nothing`
	if _, err := q.Exec(ctx, insert, string(kind), assetID); err != nil {
		return fmt.Errorf("enqueue %s job: %w", kind, err)
	}
	return nil
}

// Requeue puts a kind back into the pending state even if it already ran, which
// is what a "rebuild this derivative" path needs.
func Requeue(ctx context.Context, q Execer, kind Kind, assetID string) error {
	const upsert = `
		insert into jobs (kind, asset_id)
		values ($1, $2::uuid)
		on conflict (asset_id, kind) do update
		set state = 'pending', attempts = 0, run_after = now(),
		    locked_at = null, locked_by = null, last_error = null, updated_at = now()`
	if _, err := q.Exec(ctx, upsert, string(kind), assetID); err != nil {
		return fmt.Errorf("requeue %s job: %w", kind, err)
	}
	return nil
}

// Discard removes work that is no longer owed, whatever state it is in.
//
// The counterpart to Requeue, and the answer to a question this table cannot
// answer for itself: a job names an asset, and some work is owed to something
// larger than one. When a set of Snapchat segments is refused or taken apart,
// the merge queued against its first piece is not pending, not failed, and not
// done — it is void, and a failed row left behind would go on being reported on
// the status page as a job that gave up.
// A job that is running is left alone. Its worker is holding a lease and is
// going to report on it, and pulling the row out from under that would lose the
// completion rather than the work — the run itself is already harmless, because
// every job that can be discarded checks that it is still wanted before it
// changes anything.
func Discard(ctx context.Context, q Execer, kind Kind, assetID string) error {
	const remove = `
		delete from jobs
		where kind = $1 and asset_id = $2::uuid and state <> 'running'`
	if _, err := q.Exec(ctx, remove, string(kind), assetID); err != nil {
		return fmt.Errorf("discard %s job: %w", kind, err)
	}
	return nil
}

type Queue struct {
	pool        *pgxpool.Pool
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Lease       time.Duration
}

func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{
		pool:        pool,
		MaxAttempts: DefaultMaxAttempts,
		BaseBackoff: DefaultBaseBackoff,
		MaxBackoff:  DefaultMaxBackoff,
		Lease:       DefaultLease,
	}
}

// ErrNoJob means the queue held nothing runnable for the requested kinds. It is
// the common case on an idle server, not a failure.
var ErrNoJob = errors.New("jobs: nothing to claim")

// Claim takes the oldest runnable job of any of the given kinds and marks it
// running. Concurrent callers never receive the same job: SKIP LOCKED makes a
// claimer step over rows another transaction is already taking, rather than
// blocking behind them.
//
// Attempts is incremented here rather than on failure, so a job that kills its
// worker outright still burns an attempt and cannot loop forever.
func (q *Queue) Claim(ctx context.Context, kinds []Kind, workerID string) (Job, error) {
	if len(kinds) == 0 {
		return Job{}, ErrNoJob
	}
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}

	const claim = `
		update jobs set
			state = 'running',
			attempts = attempts + 1,
			locked_at = now(),
			locked_by = $2,
			updated_at = now()
		where id = (
			select id from jobs
			where state = 'pending'
			  and run_after <= now()
			  and kind = any($1::text[])
			order by run_after, id
			for update skip locked
			limit 1
		)
		returning id, kind, asset_id::text, attempts`

	var j Job
	err := q.pool.QueryRow(ctx, claim, names, workerID).
		Scan(&j.ID, &j.Kind, &j.AssetID, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	if err != nil {
		return Job{}, fmt.Errorf("claim job: %w", err)
	}
	return j, nil
}

// Heartbeat pushes a running job's lease forward.
//
// Without it the lease is a bet that every job finishes inside it, and a 3GB
// 4K transcode loses that bet: the sweep would hand the job to a second worker
// while ffmpeg is still running on the first, and each reclaim burns an attempt
// until a perfectly healthy job is marked failed. A worker that dies stops
// heartbeating, which is exactly when reclaiming is correct.
func (q *Queue) Heartbeat(ctx context.Context, id int64) error {
	const beat = `update jobs set locked_at = now() where id = $1 and state = 'running'`
	if _, err := q.pool.Exec(ctx, beat, id); err != nil {
		return fmt.Errorf("heartbeat job %d: %w", id, err)
	}
	return nil
}

// ReconcileMetadata queues metadata work for any asset that has none.
//
// RecordAsset enqueues in the same transaction as the insert, and the migration
// backfilled everything older, so in a healthy database this finds nothing. It
// is here for the unhealthy ones: a row restored from a manifest replay, or a
// jobs table cleared by hand.
func ReconcileMetadata(ctx context.Context, q Execer) (int64, error) {
	const insert = `
		insert into jobs (kind, asset_id)
		select 'metadata', a.id
		from assets a
		left join jobs j on j.asset_id = a.id and j.kind = 'metadata'
		where j.id is null`
	tag, err := q.Exec(ctx, insert)
	if err != nil {
		return 0, fmt.Errorf("reconcile metadata jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReconcileSignatures queues signature work for any asset whose signature is
// missing or was computed by an older version of the algorithm.
//
// Unlike the metadata reconcile above, this one is expected to find things. It
// is how a changed hash reaches an archive that has already been indexed:
// bump merge.SignatureVersion, restart, and every original is read again by the
// pool that is allowed to take an hour over it.
//
// The vault is excluded, and not as an optimisation. A signature describes what
// a photograph looks like; computing one for something in the vault would be
// this server writing down the thing the vault exists to stop it knowing.
func ReconcileSignatures(ctx context.Context, q Execer, version int) (int64, error) {
	const upsert = `
		insert into jobs (kind, asset_id)
		select 'signature', a.id
		from assets a
		left join asset_signatures sig on sig.asset_id = a.id
		where a.vault = '' and a.deleted_at is null
		  and (sig.asset_id is null or sig.version <> $1)
		on conflict (asset_id, kind) do update
		    set state = 'pending', run_after = now(), attempts = 0, last_error = null
		    where jobs.state = 'failed' or jobs.state = 'done'`
	tag, err := q.Exec(ctx, upsert, version)
	if err != nil {
		return 0, fmt.Errorf("reconcile signature jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReconcileMLPrep queues ML renditions for everything the timeline shows and
// has none.
//
// The predicate is assets_timeline_visible_idx, written out: no vault, no
// trash, no overlays, no paired videos. Those four fall out for the right
// reasons rather than as an optimisation. A paired video is not an item, it is
// a still's motion; an overlay is not a photograph, it is one layer of
// somebody's handwriting over one — and indexing either would put a second copy
// of the same moment into every result page. The vault falls out because a
// description of what a hidden photograph looks like is the thing the vault
// exists to stop this server holding.
//
// Unlike ReconcileSignatures there is no version to compare against, because
// the evidence that this job ran is a file on disk rather than a row. A changed
// rendition format is therefore a deliberate requeue rather than something a
// restart notices — which is the right way round for work whose output nothing
// is waiting for.
func ReconcileMLPrep(ctx context.Context, q Execer) (int64, error) {
	const insert = `
		insert into jobs (kind, asset_id)
		select 'mlprep', a.id
		from assets a
		left join jobs j on j.asset_id = a.id and j.kind = 'mlprep'
		where j.id is null
		  and a.vault = '' and a.deleted_at is null and not a.is_overlay
		  and a.live_parent_local_id = '' and a.live_parent_asset_id is null`
	tag, err := q.Exec(ctx, insert)
	if err != nil {
		return 0, fmt.Errorf("reconcile mlprep jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Complete marks a job done. Completed rows are kept rather than deleted: one
// row per asset per kind is a rounding error next to the blobs, and the history
// is what tells you whether a derivative was ever built.
func (q *Queue) Complete(ctx context.Context, id int64) error {
	const done = `
		update jobs
		set state = 'done', last_error = null, locked_at = null, locked_by = null,
		    updated_at = now()
		where id = $1`
	if _, err := q.pool.Exec(ctx, done, id); err != nil {
		return fmt.Errorf("complete job %d: %w", id, err)
	}
	return nil
}

// Fail records why a job failed and either reschedules it with backoff or, once
// the attempt budget is spent, parks it as failed with the error kept verbatim.
// It reports whether the failure was permanent, so the caller can update the
// asset's derived state to match.
func (q *Queue) Fail(ctx context.Context, j Job, cause error) (permanent bool, err error) {
	permanent = j.Attempts >= q.maxAttempts()
	delay := q.backoff(j.Attempts)

	const fail = `
		update jobs set
			state = case when $2 then 'failed' else 'pending' end,
			run_after = now() + make_interval(secs => $3),
			last_error = $4,
			locked_at = null,
			locked_by = null,
			updated_at = now()
		where id = $1`

	if _, err := q.pool.Exec(ctx, fail, j.ID, permanent, delay.Seconds(), cause.Error()); err != nil {
		return permanent, fmt.Errorf("fail job %d: %w", j.ID, err)
	}
	return permanent, nil
}

// ReclaimExpired returns jobs whose worker died to the pending state. A job that
// has already spent its attempts is parked as failed instead, so a file that
// reliably kills its worker cannot cycle forever.
func (q *Queue) ReclaimExpired(ctx context.Context) (int64, error) {
	const reclaim = `
		update jobs set
			state = case when attempts >= $2 then 'failed' else 'pending' end,
			last_error = coalesce(last_error, 'worker lease expired before the job reported back'),
			locked_at = null,
			locked_by = null,
			updated_at = now()
		where state = 'running'
		  and locked_at < now() - make_interval(secs => $1)`

	tag, err := q.pool.Exec(ctx, reclaim, q.lease().Seconds(), q.maxAttempts())
	if err != nil {
		return 0, fmt.Errorf("reclaim expired jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Count is one (kind, state) tally for /v1/jobs and /health.
type Count struct {
	Kind  Kind  `json:"kind"`
	State State `json:"state"`
	Count int64 `json:"count"`
}

func (q *Queue) Counts(ctx context.Context) ([]Count, error) {
	rows, err := q.pool.Query(ctx, `
		select kind, state, count(*)
		from jobs
		group by kind, state
		order by kind, state`)
	if err != nil {
		return nil, fmt.Errorf("count jobs: %w", err)
	}
	defer rows.Close()

	var counts []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Kind, &c.State, &c.Count); err != nil {
			return nil, fmt.Errorf("scan job count: %w", err)
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// Failed lists jobs that gave up, newest first, with the error that stopped
// them. This is the "what is actually broken" view.
func (q *Queue) Failed(ctx context.Context, limit int) ([]FailedJob, error) {
	rows, err := q.pool.Query(ctx, `
		select id, kind, asset_id::text, attempts, coalesce(last_error, ''), updated_at
		from jobs
		where state = 'failed'
		order by updated_at desc
		limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list failed jobs: %w", err)
	}
	defer rows.Close()

	var failed []FailedJob
	for rows.Next() {
		var f FailedJob
		if err := rows.Scan(&f.ID, &f.Kind, &f.AssetID, &f.Attempts, &f.Error, &f.FailedAt); err != nil {
			return nil, fmt.Errorf("scan failed job: %w", err)
		}
		failed = append(failed, f)
	}
	return failed, rows.Err()
}

type FailedJob struct {
	ID       int64     `json:"id"`
	Kind     Kind      `json:"kind"`
	AssetID  string    `json:"asset_id"`
	Attempts int       `json:"attempts"`
	Error    string    `json:"error"`
	FailedAt time.Time `json:"failed_at"`
}

// backoff grows exponentially from BaseBackoff to MaxBackoff, with +/-20%
// jitter so a batch of jobs failing together does not retry in lockstep.
func (q *Queue) backoff(attempt int) time.Duration {
	base, max := q.BaseBackoff, q.MaxBackoff
	if base <= 0 {
		base = DefaultBaseBackoff
	}
	if max <= 0 {
		max = DefaultMaxBackoff
	}
	if attempt < 1 {
		attempt = 1
	}

	grown := float64(base) * math.Pow(2, float64(attempt-1))
	if grown > float64(max) {
		grown = float64(max)
	}
	jittered := grown * (0.8 + 0.4*rand.Float64())
	return time.Duration(jittered)
}

func (q *Queue) maxAttempts() int {
	if q.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return q.MaxAttempts
}

func (q *Queue) lease() time.Duration {
	if q.Lease <= 0 {
		return DefaultLease
	}
	return q.Lease
}
