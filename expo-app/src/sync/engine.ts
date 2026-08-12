import {
  backoffDelay,
  CircuitBreaker,
  ITEM_BACKOFF,
  TRANSPORT_BACKOFF,
  type BackoffPolicy,
} from './backoff';
import { CHUNK_THRESHOLD } from './chunkPlan';
import {
  asSyncError,
  describable,
  emptyCounts,
  errorText,
  SyncError,
  toIso,
  type CheckRequestItem,
  type CheckResultItem,
  type Clock,
  type MediaSource,
  type OpenedAsset,
  type Phase,
  type Progress,
  type QueueItem,
  type QueueStore,
  type StateCounts,
  type Transport,
} from './types';

export type EngineConfig = {
  deviceId: string;
  /** Newest N assets to consider. 0 means the whole library. */
  maxItems: number;
  checkBatchSize: number;
  /**
   * Deliberately smaller than the check batch. Hashing freezes the JS thread per
   * item, so a big hash batch means a long stretch with nothing on the wire and
   * no repaints.
   */
  hashBatchSize: number;
  uploadConcurrency: number;
  /**
   * Originals at or above this size take the resumable path. Below it, a failed
   * upload costs one cheap retry.
   */
  chunkThreshold: number;
  itemBackoff: BackoffPolicy;
  transportBackoff: BackoffPolicy;
};

export const DEFAULT_ENGINE_CONFIG: Omit<EngineConfig, 'deviceId'> = {
  // The whole library. Phase 2 capped this at the 110-item test fixture; a real
  // backfill is the entire camera roll and the cap would silently leave most of
  // it unarchived.
  maxItems: 0,
  checkBatchSize: 200,
  hashBatchSize: 25,
  uploadConcurrency: 3,
  chunkThreshold: CHUNK_THRESHOLD,
  itemBackoff: ITEM_BACKOFF,
  transportBackoff: TRANSPORT_BACKOFF,
};

/**
 * How often progress reaches React while work is running.
 *
 * The engine reports after every item, which at 110 items is a rendered update
 * per photo and at 40,000 is the run loop spending most of its time in the
 * renderer. Coalescing to a few frames a second keeps the label honest and the
 * uploads moving.
 */
const PROGRESS_INTERVAL_MS = 250;

export type EngineDeps = {
  store: QueueStore;
  media: MediaSource;
  transport: Transport;
  clock: Clock;
  onProgress?: (progress: Progress) => void;
  onLog?: (line: string) => void;
};

/**
 * Drives the queue from enumeration to acked upload.
 *
 * The engine is the only writer to the store, and JavaScript is single-threaded,
 * so no item needs locking or an in-flight state. That is what makes a kill safe:
 * whatever state an item was last recorded in is where it resumes, and the worst
 * case is repeating one step.
 *
 * An item is only marked done once the server has acked it.
 */
export class SyncEngine {
  private readonly breaker: CircuitBreaker;
  private latestCounts: StateCounts = emptyCounts();
  private stopping = false;
  private running = false;
  private lastReportAt = 0;
  private lastPhase: Phase | null = null;

  constructor(
    private readonly deps: EngineDeps,
    private readonly config: EngineConfig
  ) {
    this.breaker = new CircuitBreaker(deps.clock, config.transportBackoff);
  }

  get isRunning(): boolean {
    return this.running;
  }

  get counts(): StateCounts {
    return this.latestCounts;
  }

  /** Asks the run loop to stop at the next safe point. */
  stop(): void {
    this.stopping = true;
  }

  async run(): Promise<StateCounts> {
    if (this.running) return this.latestCounts;
    this.running = true;
    this.stopping = false;

    try {
      await this.sweep();
      await this.enumerate();
      await this.drain();
      return this.latestCounts;
    } finally {
      this.running = false;
      await this.refreshCounts();
      if (this.stopping) {
        this.report('idle', 'Paused');
      } else {
        this.report('done', 'Up to date');
      }
    }
  }

  /** Returns failed items to the queue, keeping any hash already computed. */
  async retryFailed(): Promise<number> {
    const reset = await this.deps.store.resetFailed();
    await this.refreshCounts();
    return reset;
  }

  private async sweep(): Promise<void> {
    this.report('sweeping', 'Clearing temporary files');
    try {
      const removed = await this.deps.media.sweep();
      if (removed > 0) this.log(`swept ${removed} leftover file(s) from a previous run`);
    } catch (e) {
      // Leftover files cost disk, not correctness. Never let this stop a backup.
      this.log(`sweep failed: ${errorText(e)}`);
    }
  }

  private async enumerate(): Promise<void> {
    this.report('enumerating', 'Reading the photo library');
    const assets = await this.deps.media.enumerate(this.config.maxItems);
    const added = await this.deps.store.enqueue(assets);
    await this.refreshCounts();
    this.log(`enumerated ${assets.length} item(s), ${added} new to the queue`);
  }

  private async drain(): Promise<void> {
    while (!this.stopping) {
      if (this.breaker.isOpen()) {
        const seconds = Math.max(1, Math.ceil((this.breaker.retryAt - this.deps.clock.now()) / 1000));
        this.report('waiting', `Server unreachable — retrying in ${seconds}s`);
        await this.breaker.wait();
        continue;
      }

      const worked = await this.step();
      await this.refreshCounts();
      if (worked) continue;

      // Nothing is due, but something may still be waiting out its own backoff.
      // Sitting through that is the difference between recovering from a
      // transient error and telling the user the backup finished with items left.
      const retryAt = await this.deps.store.nextRetryAt();
      if (retryAt === null) return;
      const wait = retryAt - this.deps.clock.now();
      if (wait <= 0) {
        this.log('an item is due but was not picked up; stopping rather than spinning');
        return;
      }
      this.report('retrying', `Retrying an item in ${Math.max(1, Math.ceil(wait / 1000))}s`);
      await this.deps.clock.sleep(wait);
    }
  }

  /**
   * Advances the queue by one unit of work, returning false when nothing is due.
   *
   * The order is not the state order. Checks come first because they are cheap
   * and settle the most items; uploads come before hashing so that once the
   * server has asked for bytes, they start moving instead of waiting behind a
   * stretch of frozen-thread hashing.
   */
  private async step(): Promise<boolean> {
    const { store } = this.deps;
    const now = this.deps.clock.now();

    const pending = await store.due('pending', this.config.checkBatchSize, now);
    if (pending.length > 0) {
      await this.checkRound(pending, false);
      return true;
    }

    const hashed = await store.due('hashed', this.config.checkBatchSize, now);
    if (hashed.length > 0) {
      await this.checkRound(hashed, true);
      return true;
    }

    const wanted = await store.due('want', this.config.uploadConcurrency * 4, now);
    if (wanted.length > 0) {
      await this.uploadBatch(wanted);
      return true;
    }

    const unknown = await store.due('unknown', this.config.hashBatchSize, now);
    if (unknown.length > 0) {
      await this.hashBatch(unknown);
      return true;
    }

    return false;
  }

  /**
   * One round of sync/check. Round one sends no digest, which is the point: the
   * phone must not hash its whole library to learn the server already has it.
   */
  private async checkRound(batch: QueueItem[], withDigest: boolean): Promise<void> {
    let items = batch;
    if (withDigest) {
      // A hashed item with no digest is a bug elsewhere; send it back to be
      // hashed rather than ask the server a question it cannot answer.
      const incomplete = items.filter((item) => item.md5 === null || item.size === null);
      for (const item of incomplete) {
        await this.deps.store.update(item.localId, { state: 'unknown', nextAttemptAt: 0 });
      }
      items = items.filter((item) => item.md5 !== null && item.size !== null);
      if (items.length === 0) return;
    }

    this.report(
      'checking',
      withDigest
        ? `Checking ${items.length} item(s) by content`
        : `Checking ${items.length} item(s) against the archive`
    );

    const request: CheckRequestItem[] = items.map((item) => {
      const entry: CheckRequestItem = { localId: item.localId };
      const modifiedAt = toIso(item.modifiedAt);
      if (modifiedAt) entry.modifiedAt = modifiedAt;
      if (withDigest) {
        entry.md5 = item.md5 as string;
        entry.size = item.size as number;
      }
      return entry;
    });

    let results: CheckResultItem[];
    try {
      results = await this.deps.transport.check(this.config.deviceId, request);
    } catch (e) {
      await this.blameBatch(items, e);
      return;
    }
    this.breaker.reset();

    const byLocalId = new Map(results.map((result) => [result.localId, result]));
    for (const item of items) {
      const result = byLocalId.get(item.localId);
      if (!result) {
        await this.blameItem(item, new SyncError('the server left this item out of its answer', 'item'));
        continue;
      }
      await this.applyCheckResult(item, result, withDigest);
    }
  }

  private async applyCheckResult(
    item: QueueItem,
    result: CheckResultItem,
    withDigest: boolean
  ): Promise<void> {
    const settled = { attempts: 0, nextAttemptAt: 0, lastError: null };

    switch (result.status) {
      case 'have':
        await this.deps.store.update(item.localId, {
          ...settled,
          state: 'done',
          assetId: result.assetId ?? null,
        });
        // The archive already holds the bytes, which is exactly the case where
        // nothing else would ever describe them: a library backed up before the
        // phone knew how to say "favourite" would otherwise keep its hearts and
        // its albums to itself forever. An item reaches `done` once, so this
        // costs one request per asset in the life of the queue.
        if (result.assetId) await this.describe(item, result.assetId);
        return;
      case 'want':
        await this.deps.store.update(item.localId, { ...settled, state: 'want' });
        return;
      case 'unknown':
        // Round two supplied a digest, so the server has everything it needs and
        // "unknown" is not an answer it should give. Treat it as a request for
        // the bytes; looping the item back to hashing would never terminate.
        await this.deps.store.update(item.localId, {
          ...settled,
          state: withDigest ? 'want' : 'unknown',
        });
        return;
      default:
        await this.blameItem(
          item,
          new SyncError(`unrecognised check status ${JSON.stringify(result.status)}`, 'item')
        );
    }
  }

  private async hashBatch(items: QueueItem[]): Promise<void> {
    for (const item of items) {
      if (this.stopping) return;

      // Set before the hash begins. File.md5 is a synchronous JSI call and
      // nothing repaints while it runs, so a label written afterwards would
      // never be seen.
      this.report('hashing', `Preparing ${item.filename} — the app may freeze briefly`, true);
      await this.deps.clock.sleep(0);

      let opened: OpenedAsset;
      try {
        opened = await this.deps.media.open(item, { hash: true });
      } catch (e) {
        await this.blameItem(item, asSyncError(e, 'item'));
        continue;
      }

      try {
        if (!opened.md5) throw new SyncError('could not hash the original', 'item');
        await this.deps.store.update(item.localId, {
          state: 'hashed',
          size: opened.size,
          md5: opened.md5,
          attempts: 0,
          nextAttemptAt: 0,
          lastError: null,
        });
      } catch (e) {
        await this.blameItem(item, asSyncError(e, 'item'));
      } finally {
        await this.release(opened);
      }
    }
  }

  /**
   * Uploads with a small worker pool rather than a barrier, so a slow video does
   * not hold up the photos queued behind it. It also abandons the rest of the
   * batch once the breaker opens: if the server just went away, there is no
   * point failing every remaining item against it.
   */
  private async uploadBatch(items: QueueItem[]): Promise<void> {
    let next = 0;
    const workers = Math.min(this.config.uploadConcurrency, items.length);

    const worker = async (): Promise<void> => {
      while (!this.stopping && !this.breaker.isOpen()) {
        const index = next;
        next += 1;
        if (index >= items.length) return;
        await this.uploadOne(items[index]);
      }
    };

    await Promise.all(Array.from({ length: workers }, () => worker()));
  }

  private async uploadOne(item: QueueItem): Promise<void> {
    if (item.md5 === null || item.size === null) {
      await this.deps.store.update(item.localId, { state: 'unknown', nextAttemptAt: 0 });
      return;
    }

    const resumable = item.size >= this.config.chunkThreshold;
    this.report('uploading', `Uploading ${item.filename}`, true);

    let opened: OpenedAsset;
    try {
      opened = await this.deps.media.open(item, { hash: false });
    } catch (e) {
      await this.blameItem(item, asSyncError(e, 'item'));
      return;
    }

    try {
      // The declared digest and length have to describe the same read. If the
      // original changed since it was hashed, re-hash it instead of sending
      // bytes the server is bound to reject five times over.
      if (opened.size !== item.size) {
        await this.deps.store.update(item.localId, {
          state: 'unknown',
          size: null,
          md5: null,
          attempts: 0,
          nextAttemptAt: 0,
          lastError: `changed on disk since hashing (${item.size} -> ${opened.size} bytes)`,
        });
        return;
      }

      const request = {
        deviceId: this.config.deviceId,
        localId: item.localId,
        uri: opened.uri,
        filename: item.filename,
        md5: item.md5,
        size: item.size,
        createdAt: item.createdAt,
        modifiedAt: item.modifiedAt,
        // Only a paired video carries this, and it is exactly what stops the
        // gallery drawing a Live Photo as a photo and a silent three-second
        // clip side by side.
        liveParentLocalId: item.kind === 'live_video' ? item.parentLocalId : null,
      };

      // A big video is minutes of silence on the single-shot path. The
      // resumable one knows how far it has got, so it can say.
      const response = resumable
        ? await this.deps.transport.uploadResumable(request, (sent, total) => {
            // The last update is forced: intermediate percentages can coalesce,
            // but a label left reading 88% after the bytes are all across is
            // just wrong, and it is the one a stalled-looking upload gets
            // judged on.
            this.report('uploading', `Uploading ${item.filename} — ${percent(sent, total)}`, sent >= total);
          })
        : await this.deps.transport.upload(request);
      this.breaker.reset();

      // Only now, after the ack. A crash before this point costs one re-upload;
      // marking it earlier would cost the photo.
      await this.deps.store.update(item.localId, {
        state: 'done',
        assetId: response.id,
        attempts: 0,
        nextAttemptAt: 0,
        lastError: null,
      });
      await this.describe(item, response.id);
    } catch (e) {
      await this.blameItem(item, asSyncError(e, 'item'));
    } finally {
      await this.release(opened);
    }
  }

  /**
   * Tells the archive what the photo library knows about an asset it now holds.
   *
   * It cannot fail the item, by construction. Every caller reaches here after
   * the bytes are archived and acked, so there is nothing left to retry and
   * nothing to undo; a heart that did not make it across is worth one log line,
   * not an attempt spent or an original re-sent.
   *
   * An asset with nothing to say is not described at all — see describable(),
   * which owns that judgement so that adding a fact cannot silently fail to
   * send it.
   */
  private async describe(item: QueueItem, assetId: string): Promise<void> {
    try {
      const facts = await this.deps.media.facts(item);
      if (!facts || !describable(facts)) return;

      await this.deps.transport.describe(assetId, facts);
    } catch (e) {
      this.log(`could not record what the library knows about ${item.filename}: ${errorText(e)}`);
    }
  }

  private async blameBatch(items: QueueItem[], e: unknown): Promise<void> {
    const error = asSyncError(e, 'item');
    this.abortIfUnauthorized(error);
    this.tripIfServerFault(error);
    if (error.kind === 'unreachable') return;
    for (const item of items) await this.penalize(item, error);
  }

  private async blameItem(item: QueueItem, e: unknown): Promise<void> {
    const error = asSyncError(e, 'item');
    this.abortIfUnauthorized(error);
    this.tripIfServerFault(error);
    if (error.kind === 'unreachable') return;
    await this.penalize(item, error);
  }

  /**
   * Ends the run rather than charging anything for it.
   *
   * A revoked token fails every request identically. Backing off would waste an
   * hour arriving at the same answer, and penalizing items would walk the entire
   * library into `failed` — after which a re-pairing would fix the credential and
   * leave forty thousand items parked, needing a manual retry to come back. So
   * this throws: out of the item, out of drain(), out of run(), where the caller
   * can say what actually happened and offer the one thing that helps.
   */
  private abortIfUnauthorized(error: SyncError): void {
    if (error.kind !== 'unauthorized') return;
    this.log(`the server refused this device: ${error.message}`);
    throw error;
  }

  /**
   * A dead server opens the breaker. A 5xx opens it too and still counts against
   * the item, because a 500 on every item is an outage while a 500 on one item is
   * a bad item.
   */
  private tripIfServerFault(error: SyncError): void {
    // Deliberately not 'unauthorized': the server is answering perfectly well,
    // and holding the breaker open would frame a pairing problem as an outage.
    if (error.kind !== 'unreachable' && error.kind !== 'server') return;
    const delay = this.breaker.trip();
    this.log(`server trouble (${error.message}) — holding for ${Math.round(delay / 1000)}s`);
  }

  private async penalize(item: QueueItem, error: SyncError): Promise<void> {
    const attempts = item.attempts + 1;
    if (attempts >= this.config.itemBackoff.maxAttempts) {
      await this.deps.store.update(item.localId, {
        state: 'failed',
        attempts,
        nextAttemptAt: 0,
        lastError: error.message,
      });
      this.log(`giving up on ${item.filename}: ${error.message}`);
      return;
    }
    const delay = backoffDelay(attempts, this.config.itemBackoff, this.deps.clock.random());
    await this.deps.store.update(item.localId, {
      attempts,
      nextAttemptAt: this.deps.clock.now() + delay,
      lastError: error.message,
    });
  }

  private async release(opened: OpenedAsset): Promise<void> {
    try {
      await opened.release();
    } catch (e) {
      this.log(`could not remove a temporary copy: ${errorText(e)}`);
    }
  }

  private async refreshCounts(): Promise<void> {
    this.latestCounts = await this.deps.store.counts();
  }

  /**
   * Reports progress, coalescing repeats within a phase to a few a second.
   *
   * A change of phase always goes through: "the server is unreachable" and
   * "retrying in 30s" are the whole reason anyone is looking at this screen, and
   * dropping one because it landed too soon after the last upload line would
   * leave the app claiming to be doing something it stopped doing.
   *
   * What gets throttled is the 40,000th "Uploading IMG_4823" — same phase, new
   * label, and a re-render per item is the run loop's biggest cost at that size.
   * `force` overrides for a label written immediately before something that
   * blocks the JS thread, which would otherwise never be painted at all.
   */
  private report(phase: Phase, activity: string, force = false): void {
    const now = this.deps.clock.now();
    const changed = phase !== this.lastPhase;
    if (!force && !changed && now - this.lastReportAt < PROGRESS_INTERVAL_MS) {
      return;
    }
    this.lastReportAt = now;
    this.lastPhase = phase;

    this.deps.onProgress?.({
      phase,
      activity,
      counts: this.latestCounts,
      retryAt: this.breaker.retryAt,
    });
  }

  private log(line: string): void {
    this.deps.onLog?.(line);
  }
}

/** A whole-number percentage, for a progress label. */
function percent(sent: number, total: number): string {
  if (total <= 0) return '0%';
  return `${Math.min(100, Math.floor((sent / total) * 100))}%`;
}
