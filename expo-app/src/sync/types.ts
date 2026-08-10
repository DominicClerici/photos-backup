/**
 * The vocabulary the sync engine is built from. Everything the engine touches
 * that is slow, native, or unavailable off-device lives behind an interface
 * declared here, which is what makes the engine testable in plain Node.
 */

export type ItemKind = 'still' | 'video' | 'live_video';

/**
 * Every state is durable. There is deliberately no "in flight" state: the engine
 * is the only writer and JavaScript is single-threaded, so nothing needs
 * unwinding after a kill. An item resumes from its last recorded state and
 * repeats at most one step of work.
 *
 * pending -> unknown -> hashed -> want -> done, with failed as the terminal
 * out-of-attempts state.
 */
export type ItemState = 'pending' | 'unknown' | 'hashed' | 'want' | 'done' | 'failed';

export const ITEM_STATES: ItemState[] = ['pending', 'unknown', 'hashed', 'want', 'done', 'failed'];

/** States that still have work left. `done` is finished and `failed` is parked. */
export const RETRYABLE_STATES: ItemState[] = ['pending', 'unknown', 'hashed', 'want'];

export type QueueItem = {
  /** Opaque. A Live Photo's paired video uses `<localId>#live`. */
  localId: string;
  kind: ItemKind;
  parentLocalId: string | null;
  filename: string;
  createdAt: number | null;
  /**
   * Modification time on the phone. An edit keeps the PhotoKit identifier and
   * changes the bytes, so this is what tells the server to look again.
   */
  modifiedAt: number | null;
  size: number | null;
  md5: string | null;
  state: ItemState;
  assetId: string | null;
  attempts: number;
  nextAttemptAt: number;
  lastError: string | null;
};

export type NewItem = Pick<
  QueueItem,
  'localId' | 'kind' | 'parentLocalId' | 'filename' | 'createdAt' | 'modifiedAt'
>;

export type ItemPatch = Partial<
  Pick<
    QueueItem,
    'state' | 'size' | 'md5' | 'assetId' | 'attempts' | 'nextAttemptAt' | 'lastError' | 'modifiedAt'
  >
>;

export type StateCounts = Record<ItemState, number>;

export interface QueueStore {
  /** Adds items that are not queued yet. Existing rows keep their state. */
  enqueue(items: NewItem[]): Promise<number>;
  /**
   * Items in `state` whose next attempt is due, newest capture first — so an
   * interrupted run has secured the most recent photos rather than the oldest.
   */
  due(state: ItemState, limit: number, now: number): Promise<QueueItem[]>;
  update(localId: string, patch: ItemPatch): Promise<void>;
  /**
   * The earliest time any unfinished item becomes due, or null when none are
   * left. This is what lets the run loop wait out an item's backoff instead of
   * declaring itself finished with work still scheduled.
   */
  nextRetryAt(): Promise<number | null>;
  counts(): Promise<StateCounts>;
  failed(limit: number): Promise<QueueItem[]>;
  /**
   * Sends failed items back to the earliest state that still has useful work
   * recorded, so a retry does not redo a hash it already has.
   */
  resetFailed(): Promise<number>;
}

export type CheckStatus = 'have' | 'unknown' | 'want';

export type CheckRequestItem = {
  localId: string;
  md5?: string;
  size?: number;
  /** RFC3339, because that is what the server parses. */
  modifiedAt?: string;
};

export type CheckResultItem = {
  localId: string;
  status: CheckStatus;
  assetId?: string;
};

export type UploadRequest = {
  deviceId: string;
  localId: string;
  uri: string;
  filename: string;
  md5: string;
  size: number;
  createdAt: number | null;
  modifiedAt: number | null;
};

export type UploadResponse = {
  id: string;
  sha256: string;
  duplicate: boolean;
};

export interface Transport {
  check(deviceId: string, items: CheckRequestItem[]): Promise<CheckResultItem[]>;
  upload(request: UploadRequest): Promise<UploadResponse>;
}

export type EnumeratedAsset = NewItem;

/**
 * A local original ready to read. release() must run when the caller is done:
 * for a Live Photo's paired video it deletes the extracted copy, and skipping it
 * grows the cache without bound.
 */
export type OpenedAsset = {
  uri: string;
  size: number;
  md5: string | null;
  release: () => Promise<void>;
};

export interface MediaSource {
  enumerate(maxItems: number): Promise<EnumeratedAsset[]>;
  /**
   * Resolves and stats an original, hashing it only when asked. The hash blocks
   * the JS thread for its duration, so the caller decides when to pay for it.
   */
  open(item: QueueItem, opts: { hash: boolean }): Promise<OpenedAsset>;
  /** Removes extracted copies left behind by a previous run. */
  sweep(): Promise<number>;
}

/**
 * How a failure should be accounted for.
 *
 * `unreachable` is about the server, not the item: it opens the circuit breaker
 * and leaves every item's attempt count alone, so a ten-minute outage cannot
 * mark a whole library failed. `server` is a 5xx, which counts against both.
 * `item` is this item's problem — a 4xx, a rejected digest, an original that
 * would not resolve.
 */
export type FailureKind = 'unreachable' | 'server' | 'item';

export class SyncError extends Error {
  constructor(
    message: string,
    readonly kind: FailureKind,
    readonly status?: number
  ) {
    super(message);
    this.name = 'SyncError';
  }
}

export type Clock = {
  now: () => number;
  sleep: (ms: number) => Promise<void>;
  /** In [0, 1). Injected so backoff is deterministic under test. */
  random: () => number;
};

export const systemClock: Clock = {
  now: () => Date.now(),
  sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  random: () => Math.random(),
};

export type Phase =
  | 'idle'
  | 'sweeping'
  | 'enumerating'
  | 'checking'
  | 'hashing'
  | 'uploading'
  /** The circuit breaker is open: the server is not answering. */
  | 'waiting'
  /** The server is fine; an individual item is serving out its backoff. */
  | 'retrying'
  | 'done';

export type Progress = {
  phase: Phase;
  activity: string;
  counts: StateCounts;
  /** When the circuit breaker will let work resume; 0 when it is closed. */
  retryAt: number;
};

export function emptyCounts(): StateCounts {
  return { pending: 0, unknown: 0, hashed: 0, want: 0, done: 0, failed: 0 };
}

export function errorText(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

/** Wraps anything thrown into a SyncError, defaulting to the given blame. */
export function asSyncError(e: unknown, fallback: FailureKind): SyncError {
  if (e instanceof SyncError) return e;
  return new SyncError(errorText(e), fallback);
}

export function toIso(ms: number | null): string | undefined {
  if (ms === null || !Number.isFinite(ms)) return undefined;
  return new Date(ms).toISOString();
}
