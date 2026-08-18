import type { PhotoKitFacts } from '../../../modules/photo-facts';
import type {
  AssetFacts,
  CheckRequestItem,
  CheckResultItem,
  Clock,
  EnumeratedAsset,
  MediaSource,
  OpenedAsset,
  QueueItem,
  Transport,
  UploadProgress,
  UploadRequest,
  UploadResponse,
} from '../types';
import { SyncError } from '../types';

/**
 * A clock whose sleep() jumps forward instead of waiting. Backoff and the
 * circuit breaker are then exercised at full speed and deterministically —
 * without this, a test of five retries would take ten real seconds.
 */
export class TestClock implements Clock {
  private current: number;

  constructor(
    start = 1_700_000_000_000,
    private readonly fixedRandom = 0.5
  ) {
    this.current = start;
  }

  now = (): number => this.current;

  sleep = async (ms: number): Promise<void> => {
    this.current += Math.max(0, ms);
  };

  random = (): number => this.fixedRandom;

  advance(ms: number): void {
    this.current += ms;
  }
}

export type CheckCall = { deviceId: string; items: CheckRequestItem[] };

type CheckResponder = (call: CheckCall, callIndex: number) => CheckResultItem[];
type UploadResponder = (request: UploadRequest, callIndex: number) => UploadResponse;

export class FakeTransport implements Transport {
  readonly checkCalls: CheckCall[] = [];
  readonly uploads: UploadRequest[] = [];
  /** Local ids that went down the resumable path rather than single-shot. */
  readonly resumable: string[] = [];
  readonly described: { assetId: string; facts: AssetFacts }[] = [];
  /** Set to make describe() throw, which must never reach an item. */
  describeFails = false;

  constructor(
    private readonly checkResponder: CheckResponder,
    private readonly uploadResponder: UploadResponder = defaultUpload
  ) {}

  async describe(assetId: string, facts: AssetFacts): Promise<void> {
    if (this.describeFails) throw new SyncError('describe blew up', 'item');
    this.described.push({ assetId, facts });
  }

  async check(deviceId: string, items: CheckRequestItem[]): Promise<CheckResultItem[]> {
    const call = { deviceId, items };
    const index = this.checkCalls.length;
    this.checkCalls.push(call);
    return this.checkResponder(call, index);
  }

  async upload(request: UploadRequest): Promise<UploadResponse> {
    const index = this.uploads.length;
    this.uploads.push(request);
    return this.uploadResponder(request, index);
  }

  async uploadResumable(
    request: UploadRequest,
    onProgress?: UploadProgress
  ): Promise<UploadResponse> {
    const index = this.uploads.length;
    this.uploads.push(request);
    this.resumable.push(request.localId);

    // Two updates, so a test can tell that progress was reported at all and
    // that it ended at the full size.
    onProgress?.(Math.floor(request.size / 2), request.size);
    onProgress?.(request.size, request.size);

    return this.uploadResponder(request, index);
  }
}

let uploadCounter = 0;

function defaultUpload(request: UploadRequest): UploadResponse {
  uploadCounter += 1;
  return { id: `asset-${uploadCounter}`, sha256: `sha-${request.localId}`, duplicate: false };
}

/** Answers every item in a check with the same status. */
export function alwaysRespond(status: 'have' | 'unknown' | 'want'): CheckResponder {
  return ({ items }) =>
    items.map((item) => ({
      localId: item.localId,
      status,
      ...(status === 'have' ? { assetId: `asset-for-${item.localId}` } : {}),
    }));
}

/** Round one answers `unknown`, round two answers `second`. */
export function twoRound(second: 'have' | 'want'): CheckResponder {
  return ({ items }) =>
    items.map((item) => {
      const hasDigest = item.md5 !== undefined;
      if (!hasDigest) return { localId: item.localId, status: 'unknown' as const };
      return second === 'have'
        ? { localId: item.localId, status: 'have' as const, assetId: `asset-for-${item.localId}` }
        : { localId: item.localId, status: 'want' as const };
    });
}

export type FakeFile = { size: number; md5: string };

export class FakeMedia implements MediaSource {
  readonly opens: { localId: string; hash: boolean }[] = [];
  readonly releases: string[] = [];
  sweepCount = 0;
  /** What facts() answers, per local id. Anything absent has none. */
  readonly facts_: Map<string, AssetFacts> = new Map();

  constructor(
    private readonly assets: EnumeratedAsset[] = [],
    private readonly files: Map<string, FakeFile> = new Map(),
    private readonly openFailures: Set<string> = new Set()
  ) {}

  async enumerate(): Promise<EnumeratedAsset[]> {
    return this.assets;
  }

  async open(item: QueueItem, opts: { hash: boolean }): Promise<OpenedAsset> {
    this.opens.push({ localId: item.localId, hash: opts.hash });
    if (this.openFailures.has(item.localId)) {
      throw new SyncError(`cannot open ${item.localId}`, 'item');
    }
    const file = this.files.get(item.localId) ?? { size: 100, md5: `md5-${item.localId}` };
    return {
      uri: `file:///fake/${item.localId}`,
      size: file.size,
      md5: opts.hash ? file.md5 : null,
      release: async () => {
        this.releases.push(item.localId);
      },
    };
  }

  async facts(item: QueueItem): Promise<AssetFacts | null> {
    if (item.kind === 'live_video') return null;
    return this.facts_.get(item.localId) ?? null;
  }

  async sweep(): Promise<number> {
    return this.sweepCount;
  }

  hashOpens(): string[] {
    return this.opens.filter((open) => open.hash).map((open) => open.localId);
  }
}

/**
 * A set of library facts to hang on a fake asset. The default is the shape a
 * build without the PhotoFacts native module produces: what
 * expo-media-library can answer, and nothing from PHAsset itself.
 */
export function facts(overrides: Partial<AssetFacts> = {}): AssetFacts {
  return { favorite: false, subtypes: [], albums: [], location: null, photoKit: null, ...overrides };
}

/**
 * What the native module answers for an unremarkable photo, so a test can vary
 * the one field it is about.
 */
export function photoKit(overrides: Partial<PhotoKitFacts> = {}): PhotoKitFacts {
  return {
    localId: 'ph://asset',
    hidden: false,
    favorite: false,
    mediaType: { value: 1, name: 'image' },
    mediaSubtypes: { value: 0, names: [] },
    sourceType: { value: 1, names: ['typeUserLibrary'] },
    playbackStyle: { value: 1, name: 'image' },
    burstIdentifier: null,
    burstSelectionTypes: { value: 0, names: [] },
    representsBurst: false,
    pixelWidth: 4032,
    pixelHeight: 3024,
    durationSeconds: 0,
    createdAt: '2025-01-06T01:38:05.000Z',
    modifiedAt: '2025-01-06T01:38:05.000Z',
    hasAdjustments: false,
    originalFilename: 'IMG_5874.HEIC',
    resources: [
      {
        type: { value: 1, name: 'photo' },
        originalFilename: 'IMG_5874.HEIC',
        uniformTypeIdentifier: 'public.heic',
      },
    ],
    location: null,
    ...overrides,
  };
}

export function asset(localId: string, overrides: Partial<EnumeratedAsset> = {}): EnumeratedAsset {
  return {
    localId,
    kind: 'still',
    parentLocalId: null,
    filename: `${localId}.HEIC`,
    createdAt: 1_600_000_000_000,
    modifiedAt: 1_600_000_000_000,
    ...overrides,
  };
}

export function queued(localId: string, overrides: Partial<QueueItem> = {}): QueueItem {
  return {
    localId,
    kind: 'still',
    parentLocalId: null,
    filename: `${localId}.HEIC`,
    createdAt: 1_600_000_000_000,
    modifiedAt: 1_600_000_000_000,
    size: null,
    md5: null,
    state: 'pending',
    assetId: null,
    attempts: 0,
    nextAttemptAt: 0,
    lastError: null,
    ...overrides,
  };
}
