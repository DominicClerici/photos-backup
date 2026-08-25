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
	// KindVision is one call to photo-ml per asset: the ML renditions go out
	// over loopback and 1152 numbers per frame come back. It is the Python half
	// of ML_IMAGES.md §3, and the only kind of work in this table that depends
	// on a process the archive is allowed not to have.
	//
	// Its own pool, for the reason KindSignature has one and with a stronger
	// case: it is a pass over the whole archive that nothing is waiting for,
	// and it is a queue in front of one GPU, so more claimants would only mean
	// more processes waiting on the same card.
	//
	// The kind that is deferred rather than failed. Every other job here can
	// only be stopped by something wrong with a photograph; this one can be
	// stopped by a service being restarted, and Defer below is what keeps an
	// outage from marking sixty thousand perfectly good photographs broken.
	KindVision Kind = "vision"
	// KindOCR is a dedicated text recogniser over the same renditions: what a
	// screenshot says, what is on the receipt, what the road sign read.
	//
	// Its own kind rather than part of KindDescribe, because it is a different
	// model with a different cost — minutes over the library against hours —
	// and because it is the one of the two that runs on the CPU. Folding them
	// together would mean a captioner swap re-reading every screenshot in the
	// archive to learn nothing new.
	KindOCR Kind = "ocr"
	// KindDescribe is the captioner: a sentence and a handful of free-form tags
	// per frame, from a 4B vision-language model.
	//
	// The expensive pass in the whole system — hours, where re-embedding the
	// library is fifteen minutes — which is exactly why it is separate from
	// KindVision. One job for both would tie every encoder bench to a full
	// re-captioning, the same coupling ML_IMAGES.md §5 split mlprep out to
	// avoid, one step further along.
	//
	// Unlike every other kind here, there is no reconcile that queues this on
	// startup. A restart may not begin four hours of GPU work: the backfill is
	// `photobackup ml backfill`, typed deliberately. New uploads are queued by
	// the mlprep job the ordinary way.
	KindDescribe Kind = "describe"
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
	// FollowOn is false when whoever queued this wanted its output and not the
	// work that usually follows it. Only the bulk re-render sets it; see
	// RequeueMLPrep and migration 0021.
	FollowOn bool
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

// Defer puts a running job back without spending an attempt on it.
//
// The third thing that can happen to a claimed job, and it exists because of
// one that is optional. Complete says the work is done and Fail says the work
// was tried and did not go; Defer says the work was never tried, because the
// thing that does it was not there.
//
// Rolling the attempt back is the whole point and it is not bookkeeping. An
// attempt is a claim on the file — five of them mean five real goes at the same
// bytes, which is how a genuinely broken original gives up within the hour
// instead of burning ffmpeg forever. A job that never reached the bytes has not
// used one. Without this, restarting photo-ml during a backfill would take five
// swings at a closed socket for every queued asset and park the lot as
// permanently failed, and the way back would be a hand-written UPDATE against
// sixty thousand rows.
//
// The delay is the caller's, because only the caller knows what it is waiting
// for. The running guard is for the job the lease sweep reclaimed while this
// was deciding: that row belongs to the queue again, and writing to it here
// would put a stale run_after on work another worker may already have.
func (q *Queue) Defer(ctx context.Context, j Job, delay time.Duration, reason string) error {
	const put = `
		update jobs set
			state = 'pending',
			attempts = greatest(attempts - 1, 0),
			run_after = now() + make_interval(secs => $2),
			last_error = $3,
			locked_at = null,
			locked_by = null,
			updated_at = now()
		where id = $1 and state = 'running'`
	if _, err := q.pool.Exec(ctx, put, j.ID, delay.Seconds(), reason); err != nil {
		return fmt.Errorf("defer job %d: %w", j.ID, err)
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
	return q.claim(ctx, kinds, workerID, false)
}

// ClaimInOrder is Claim for a pool whose kinds are a priority list rather than
// a set: every runnable job of the first kind goes before any job of the
// second.
//
// One pool uses it, and it is the pool in front of the GPU. Its three kinds cost
// wildly different amounts — fifteen minutes to embed the library, twenty to
// read the text in it, four hours to caption it — and FIFO across the three
// would interleave them so that all three finish at the end. Draining in order
// means the fifteen-minute pass is done fifteen minutes in, and the four-hour
// one runs on a machine that has nothing else to do. It is also what keeps the
// captioner and the recogniser from thrashing the card by loading and unloading
// past each other.
//
// Starvation is the obvious objection and it does not apply here: these queues
// are finite passes over a library that is not growing during a backfill, and a
// photograph uploaded mid-run jumps to the front of all three rather than
// waiting behind the caption pass — which is the right way round.
//
// Every other pool keeps Claim's plain FIFO, deliberately. The transcode pool
// holds playbacks and merges, neither of which anybody is waiting for, and
// ranking one over the other would be a behaviour change made as a side effect
// of something unrelated.
func (q *Queue) ClaimInOrder(ctx context.Context, kinds []Kind, workerID string) (Job, error) {
	return q.claim(ctx, kinds, workerID, true)
}

func (q *Queue) claim(ctx context.Context, kinds []Kind, workerID string, ordered bool) (Job, error) {
	if len(kinds) == 0 {
		return Job{}, ErrNoJob
	}
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}

	order := "run_after, id"
	if ordered {
		order = "array_position($1::text[], kind), run_after, id"
	}

	claim := `
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
			order by ` + order + `
			for update skip locked
			limit 1
		)
		returning id, kind, asset_id::text, attempts, follow_on`

	var j Job
	err := q.pool.QueryRow(ctx, claim, names, workerID).
		Scan(&j.ID, &j.Kind, &j.AssetID, &j.Attempts, &j.FollowOn)
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

// RequeueMLPrep re-renders the whole library, and is what a changed MLEdge or a
// changed rendition format is.
//
// ReconcileMLPrep deliberately cannot do this: its predicate is "has no mlprep
// row", because the evidence this job ran is a file on disk rather than a row
// and there is no version to compare against. That is the right default — it
// means a restart never silently re-decodes 73GB of originals — and it is
// exactly why the other half has to be typed. `photobackup ml renditions`.
//
// What it does not do is queue any of the three passes that read the renditions,
// and follow_on = false is how it says so. Re-rendering is an hour of CPU;
// re-captioning is four hours of GPU, and it should not arrive as a side effect
// of asking for the first. Each pass is asked for by name — `ml backfill --kind
// ocr` for the recogniser, a delete from asset_embeddings and a restart for the
// encoder.
//
// That containment used to be left to Enqueue's do-nothing-on-conflict, on the
// reasoning that an asset already embedded, read and captioned would keep its
// rows and so keep its results. The hole in it was the asset that had never been
// captioned at all: no row to conflict with, so the re-render created one, and
// a bounded captioning backfill means most of the archive is in that state. An
// hour of CPU quietly queued four hours of GPU after all — and worse than
// merely queued, because the describe jobs arrived interleaved with the ocr
// jobs rather than behind them, which is the ordering app.py's captioner
// eviction assumes. See migration 0021.
//
// Pending rows are adopted rather than skipped, and that is the second half of
// the same fix. This guard read `state = 'failed' or state = 'done'` — which
// left an already-pending mlprep row exactly as it was, follow_on included. A
// re-render is eleven hours of CPU that somebody will interrupt, and the obvious
// thing to do afterwards is type the command again; if that second run cannot
// reach the rows the first one left pending, most of the archive keeps whatever
// intent it was queued with and the containment above applies to the minority.
// The cost is an upload whose renditions were owed at the moment somebody typed
// this, which loses its automatic caption and is picked up by the `ml backfill`
// that follows a re-render anyway. Running rows are still left alone: that is a
// worker mid-decode, and the lease sweep owns it.
func RequeueMLPrep(ctx context.Context, q Execer) (int64, error) {
	const upsert = `
		insert into jobs (kind, asset_id, follow_on)
		select 'mlprep', a.id, false
		from assets a
		where a.vault = '' and a.deleted_at is null and not a.is_overlay
		  and a.live_parent_local_id = '' and a.live_parent_asset_id is null
		on conflict (asset_id, kind) do update
		    set state = 'pending', run_after = now(), attempts = 0,
		        locked_at = null, locked_by = null, last_error = null,
		        follow_on = false, updated_at = now()
		    where jobs.state <> 'running'`
	tag, err := q.Exec(ctx, upsert)
	if err != nil {
		return 0, fmt.Errorf("requeue mlprep jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReconcileVision queues an embedding pass for everything that has renditions
// and no vector from the named model.
//
// The mlprep job being done is the predicate, and it is doing two jobs. It is
// how this finds the backfill — seventeen thousand assets whose renditions were
// written by a pool that finished hours ago and queued nothing. And it is the
// dependency: there is no point handing photo-ml an asset whose 512px WebP has
// not been written yet, and the timeline exclusions that decide which assets get
// one are already baked into the mlprep row rather than restated here.
//
// The model is the version, in the sense ReconcileSignatures means it. Swapping
// encoders is `delete from asset_embeddings where model = <old>` and a restart:
// the not-exists goes true again for every asset, the done rows go back to
// pending, and fifteen minutes later the library has been described by a
// different model. Never a migration, and the two models can sit in the table
// together while somebody measures one against the other.
//
// One honest cost. A video that could not be sampled has no renditions, so its
// vision job finds nothing to send, completes, and writes no embedding — which
// makes it indistinguishable here from one that has never run, and it is
// offered again on every start. It costs a claim and a stat per restart for a
// handful of clips, and the alternative is a row recording that a model looked
// at nothing, which is a heavier thing to carry than the churn.
func ReconcileVision(ctx context.Context, q Execer, model string) (int64, error) {
	const upsert = `
		insert into jobs (kind, asset_id)
		select 'vision', a.id
		from assets a
		join jobs prep on prep.asset_id = a.id
		     and prep.kind = 'mlprep' and prep.state = 'done'
		where a.vault = '' and a.deleted_at is null
		  and not exists (
		      select 1 from asset_embeddings e
		      where e.asset_id = a.id and e.model = $1)
		on conflict (asset_id, kind) do update
		    set state = 'pending', run_after = now(), attempts = 0, last_error = null
		    where jobs.state = 'failed' or jobs.state = 'done'`
	tag, err := q.Exec(ctx, upsert, model)
	if err != nil {
		return 0, fmt.Errorf("reconcile vision jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Unbounded asks QueueWords for every asset that is owed work.
const Unbounded = -1

// QueueWords queues the captioner or the text recogniser, newest first, and
// bounded. By default over assets nothing has described yet; with force, over
// every asset in scope whether it has been described or not.
//
// It is a command rather than a reconcile, and that is the difference between
// this and ReconcileVision. Embedding the library is fifteen minutes and is a
// reasonable consequence of a restart. Captioning it is four hours of GPU, and a
// `systemctl restart photod` that quietly begins four hours of work is a service
// restart with a surprise in it — the same objection migrations 0016 and 0017
// made to queueing from a migration, applied to the other thing that runs
// without anybody typing. `photobackup ml backfill` is where it is typed.
//
// Bounded separately for stills and videos, because that is how a sample is
// asked for: a thousand photographs and twenty clips is an evening's worth of
// vocabulary to build a search page against, and it is a different question from
// "how many assets". Unbounded for either means all of them.
//
// Newest first, because a bounded run is a sample and the most recent
// photographs are the ones somebody is about to search for.
//
// The conflict clause requeues what already ran, so a captioner swap is this
// command and never a migration.
//
// force is what makes that true for a changed *recipe* rather than a changed
// model — a raised captioner.MAX_PIXELS, a rewritten prompt, better OCR
// weights under the same name. Without it those are invisible here: the filter
// this drops asks whether a row exists, not whether the row is any good, so
// re-running the pass over a library that already has captions queues exactly
// nothing.
//
// It does not delete anything first, and that is the point rather than an
// omission. PutDescription and PutOCR both upsert, putTags clears and rewrites
// the set, and each write refreshes its own asset_search row — so a forced pass
// replaces each photograph's words in one transaction, as it reaches it. The
// obvious alternative is a delete followed by this command, and it is worse in
// the way that matters: it would leave the library with no captions at all for
// however many hours the pass takes, and a search box that has quietly lost its
// index is a worse failure than one whose answers are a few hours stale.
//
// What it costs is that this is now a command that can spend a whole night of
// GPU on work nobody needed, so the caller says --force out loud and mlcmd
// prints what it is about to redo.
func QueueWords(ctx context.Context, q Execer, kind Kind, model string, stills, videos int, force bool) (int64, error) {
	var written string
	switch kind {
	case KindDescribe:
		written = `select 1 from asset_descriptions d where d.asset_id = a.id and d.model = $1`
	case KindOCR:
		written = `select 1 from asset_ocr o where o.asset_id = a.id and o.model = $1`
	default:
		return 0, fmt.Errorf("queue words: %q is not a captioning kind", kind)
	}

	// The mlprep job being done is the dependency and the scope at once: there
	// is no point handing photo-ml an asset whose renditions have not been
	// written, and the timeline's exclusions — vault, trash, overlays, paired
	// videos — are already baked into which assets have an mlprep row rather
	// than restated here.
	insert := `
		with candidates as (
			select a.id, a.media_kind,
			       row_number() over (
			           partition by a.media_kind
			           order by a.sort_time desc, a.id desc) as rank
			from assets a
			join jobs prep on prep.asset_id = a.id
			     and prep.kind = 'mlprep' and prep.state = 'done'
			where a.vault = '' and a.deleted_at is null
			  and (not exists (` + written + `) or $5)
		)
		insert into jobs (kind, asset_id)
		select $2, id from candidates
		where (media_kind = 'image' and ($3 < 0 or rank <= $3))
		   or (media_kind = 'video' and ($4 < 0 or rank <= $4))
		on conflict (asset_id, kind) do update
		    set state = 'pending', run_after = now(), attempts = 0, last_error = null
		    where jobs.state = 'failed' or jobs.state = 'done'`

	tag, err := q.Exec(ctx, insert, model, string(kind), stills, videos, force)
	if err != nil {
		return 0, fmt.Errorf("queue %s jobs: %w", kind, err)
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

// Quiet reports whether the named kinds have nothing left to do and have not
// finished anything within the last `within`.
//
// The question a pool asks before taking work that must not run beside those
// kinds. Two conditions rather than one, and the second is the whole point: an
// empty queue is not a quiet one when an upload lands every two seconds. The
// pending check alone would let a caption start in the gap between two
// photographs arriving, which is exactly the interleaving that has to be
// avoided — see worker.visionHold and photo_ml/residency.py.
//
// A `within` of zero asks only the first question: is there unfinished work of
// these kinds right now.
//
// Failed rows are deliberately not counted. They are parked, nothing will claim
// them, and letting a permanently broken job hold off another pass forever would
// be the wrong kind of caution.
func (q *Queue) Quiet(ctx context.Context, kinds []Kind, within time.Duration) (bool, error) {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}

	const query = `
		select not exists (
		    select 1 from jobs
		    where kind = any($1::text[]) and state in ('pending', 'running')
		) and not exists (
		    select 1 from jobs
		    where kind = any($1::text[]) and state = 'done'
		      and updated_at > now() - make_interval(secs => $2)
		)`

	var quiet bool
	if err := q.pool.QueryRow(ctx, query, names, within.Seconds()).Scan(&quiet); err != nil {
		return false, fmt.Errorf("ask whether %v are quiet: %w", kinds, err)
	}
	return quiet, nil
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
