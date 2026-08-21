/**
 * The vocabulary the sync engine is built from. Everything the engine touches
 * that is slow, native, or unavailable off-device lives behind an interface
 * declared here, which is what makes the engine testable in plain Node.
 */

import type { PhotoKitFacts } from '../../modules/photo-facts';

export type ItemKind = 'still' | 'video' | 'live_video';

/**
 * Where an item's bytes come from, which is a different question from what kind
 * of thing it is.
 *
 * `library` is the camera roll: the original is on the flash of this phone and
 * opening it is a file read. `shared` is an iCloud Shared Album, where there is
 * no original on the phone at all — only Apple's rendition of somebody else's
 * upload, reachable by asking iCloud for it over the network.
 *
 * It is stored rather than derived because the difference outlives the run that
 * discovered it: an item queued today is opened by a run tomorrow, and nothing
 * about a local identifier says which of the two it names.
 */
export type ItemSource = 'library' | 'shared';

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
  source: ItemSource;
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
  'localId' | 'kind' | 'source' | 'parentLocalId' | 'filename' | 'createdAt' | 'modifiedAt'
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
  /**
   * Sends every finished item back to be checked against the archive again.
   *
   * `done` is the one state nothing else can leave: enqueue() is `insert or
   * ignore` on the local id, so re-enumeration skips the row, and due() never
   * selects it. That is correct while the phone's memory and the archive agree,
   * and a trap when they stop — an archive rebuilt underneath the phone leaves
   * rows claiming a backup that no longer exists, and the run reports "up to
   * date" while sending nothing.
   *
   * This makes the server the authority again rather than trusting either side:
   * everything the archive still holds is answered `have` and settles straight
   * back to done, and only what is genuinely missing goes on to upload.
   */
  reopenDone(): Promise<number>;
  /**
   * Drops unfinished shared items that this enumeration did not offer, and
   * returns how many went.
   *
   * What makes it necessary is that the queue is additive: enqueue() is `insert
   * or ignore`, so a row that entered it once stays until it is done or failed.
   * That is right for the camera roll, where a photograph nobody deleted is
   * still there next run. It is wrong for a shared album, where two things
   * happen that have no equivalent locally — somebody unticks the album, and
   * somebody else removes a photograph from it. Without this, an unticked album
   * would go on uploading everything a previous run had already queued out of
   * it, which is the checkbox meaning nothing.
   *
   * `done` rows are kept, and they are the reason this can be safe: they are the
   * record that the archive holds the photograph, so re-ticking the album settles
   * them in one check rather than fetching them all again. Everything dropped is
   * work that had not finished, and dropping it costs at most doing that work
   * later.
   */
  pruneShared(keep: string[]): Promise<number>;
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
  /**
   * For a Live Photo's paired video, the local id of the still it belongs to.
   * The phone is the only party that can know this — the two files share
   * nothing but a capture time — and without it the server archives the clip as
   * a separate item and the gallery shows the same moment twice.
   */
  liveParentLocalId?: string | null;
};

export type UploadResponse = {
  id: string;
  sha256: string;
  duplicate: boolean;
};

/**
 * What the photo library knows about an asset that the asset's own bytes do
 * not, and that nothing else can recover once the phone is wiped or the photo
 * is deleted from it.
 *
 * A heart, an album and "this is a screenshot" are decisions a person made, not
 * facts a camera recorded — no amount of re-reading the original brings them
 * back. They are the phone's version of a Takeout's sidecar, and they travel
 * the same way: a separate request after the bytes are safe, never a header on
 * the upload, because a caption or an album name has no business in one and
 * losing the description must never cost the photograph.
 *
 * The location is here for the small number of originals that carry none of
 * their own — anything a messaging app rewrote, anything saved out of another
 * app. The file's own coordinates outrank it wherever it has some.
 */
export type AssetFacts = {
  favorite: boolean;
  /** PhotoKit's subtypes: 'screenshot', 'livePhoto', 'panorama', 'hdr', … */
  subtypes: string[];
  /** Album titles. PhotoKit lets one asset be in several. */
  albums: string[];
  location: AssetLocation | null;
  /**
   * Everything else PHAsset answered, straight from the native module — the
   * Hidden album, the burst, the source, the resource inventory, the rest of
   * the fix. Null when this build has no PhotoFacts module in it, which is the
   * ordinary state of a JS reload against an older dev client.
   */
  photoKit: PhotoKitFacts | null;
};

/**
 * Where the library says the photo was taken.
 *
 * Two coordinates is all expo-media-library reports and all this had until the
 * native module arrived, so everything past them is optional: an asset described
 * by an older build carries a pair of numbers and nothing else.
 */
export type AssetLocation = {
  latitude: number;
  longitude: number;
  altitude?: number;
  horizontalAccuracy?: number;
  verticalAccuracy?: number;
  course?: number;
  speed?: number;
  /** When CoreLocation took the fix, which is not when the shutter fired. */
  timestamp?: string | null;
};

/**
 * Whether an asset is worth a request of its own.
 *
 * The old rule was a list of the four things the library could report — a heart,
 * a subtype, an album, a location — and it was already one field away from being
 * wrong: adding the Hidden album to AssetFacts without touching it would have
 * left every hidden-but-otherwise-unremarkable photo silently undescribed,
 * losing exactly the fact the native module was built to capture.
 *
 * So anything the native module answered for counts, without enumerating what it
 * said. That does mean a request per asset rather than per interesting asset,
 * which on a library of tens of thousands is tens of thousands of small posts to
 * a server on the same LAN, once each in the life of the queue. It buys two
 * things worth more than that: an edit history and a source type for every asset
 * rather than for the photogenic ones, and a rule that cannot rot the next time
 * somebody captures a new fact and forgets this function exists.
 */
export function describable(facts: AssetFacts): boolean {
  if (facts.photoKit !== null) return true;
  return (
    facts.favorite ||
    facts.subtypes.length > 0 ||
    facts.albums.length > 0 ||
    facts.location !== null
  );
}

/** Fraction of one original that has reached the server, for the progress line. */
export type UploadProgress = (sent: number, total: number) => void;

export interface Transport {
  check(deviceId: string, items: CheckRequestItem[]): Promise<CheckResultItem[]>;
  upload(request: UploadRequest): Promise<UploadResponse>;
  /**
   * The resumable path, for originals big enough that restarting one is a real
   * cost. Asks the server where it got to and sends only the rest, so a video
   * interrupted by a dropped connection or an app kill continues rather than
   * starting over.
   */
  uploadResumable(request: UploadRequest, onProgress?: UploadProgress): Promise<UploadResponse>;
  /**
   * Records what the library knows about an archived asset. Separate from the
   * upload because it describes bytes that are already safe, and because it is
   * also the only way an asset the server already had ever gets described.
   */
  describe(assetId: string, facts: AssetFacts): Promise<void>;
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
  /**
   * What the file should be called on the way to the archive, where opening it
   * turned out to know better than the queue did.
   *
   * Only a shared asset sets it, and the reason is the archive's file naming: a
   * shared HEIC comes back from iCloud re-encoded, PhotoKit goes on calling the
   * asset IMG_4021.HEIC, and the server trusts a recognised extension over the
   * bytes. Left alone, every shared JPEG in the archive would be stored and
   * served as a HEIC. The resource Apple actually handed over knows its own
   * name; this is it.
   */
  filename?: string;
  release: () => Promise<void>;
};

export interface MediaSource {
  enumerate(maxItems: number): Promise<EnumeratedAsset[]>;
  /**
   * Resolves and stats an original, hashing it only when asked. The hash blocks
   * the JS thread for its duration, so the caller decides when to pay for it.
   */
  open(item: QueueItem, opts: { hash: boolean }): Promise<OpenedAsset>;
  /**
   * Reads the library's own record of an asset — see AssetFacts. Returns null
   * for anything that has none of its own, which is every Live Photo's paired
   * video: PhotoKit holds one asset there, and the still is it.
   */
  facts(item: QueueItem): Promise<AssetFacts | null>;
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
 *
 * `unauthorized` is neither. The server is healthy and the item is fine; this
 * phone is not allowed to write. Retrying cannot help and backoff would only
 * delay saying so, so it ends the run instead of being charged to anything —
 * without it, a revoked token would quietly walk the whole library into `failed`
 * five attempts at a time.
 */
export type FailureKind = 'unreachable' | 'server' | 'item' | 'unauthorized';

/** True when a failure means this device needs pairing again. */
export function isUnauthorized(e: unknown): boolean {
  return e instanceof SyncError && e.kind === 'unauthorized';
}

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
