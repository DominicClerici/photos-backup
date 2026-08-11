import { ITEM_BACKOFF } from '../backoff';
import { DEFAULT_ENGINE_CONFIG, SyncEngine, type EngineConfig } from '../engine';
import { MemoryQueueStore } from '../memoryStore';
import { SyncError, type Phase, type Progress, type QueueItem, type UploadRequest } from '../types';
import {
  alwaysRespond,
  asset,
  FakeMedia,
  FakeTransport,
  queued,
  TestClock,
  twoRound,
} from './fakes';

const DEVICE = 'ios-test';

type Harness = {
  engine: SyncEngine;
  store: MemoryQueueStore;
  media: FakeMedia;
  transport: FakeTransport;
  clock: TestClock;
  phases: Phase[];
  logs: string[];
};

function build(options: {
  transport: FakeTransport;
  media?: FakeMedia;
  seed?: QueueItem[];
  config?: Partial<EngineConfig>;
}): Harness {
  const store = new MemoryQueueStore(options.seed ?? []);
  const media = options.media ?? new FakeMedia();
  const clock = new TestClock();
  const phases: Phase[] = [];
  const logs: string[] = [];

  const engine = new SyncEngine(
    {
      store,
      media,
      transport: options.transport,
      clock,
      onProgress: (progress: Progress) => phases.push(progress.phase),
      onLog: (line: string) => logs.push(line),
    },
    { ...DEFAULT_ENGINE_CONFIG, deviceId: DEVICE, ...options.config }
  );

  return { engine, store, media, transport: options.transport, clock, phases, logs };
}

function statesOf(store: MemoryQueueStore): Record<string, string> {
  const states: Record<string, string> = {};
  for (const item of store.snapshot()) states[item.localId] = item.state;
  return states;
}

// The Phase 2 exit criterion. A second run must settle everything in round one:
// no digests sent, so nothing on the phone is hashed and nothing is uploaded.
test('a second run neither hashes nor uploads anything', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    media: new FakeMedia([asset('a'), asset('b'), asset('c')]),
  });

  await h.engine.run();

  expect(h.engine.counts.done).toBe(3);
  expect(h.transport.uploads).toHaveLength(0);
  expect(h.media.opens).toHaveLength(0);
  expect(h.transport.checkCalls).toHaveLength(1);
  expect(h.transport.checkCalls[0].items.every((item) => item.md5 === undefined)).toBe(true);
});

test('an unknown item is hashed, wanted, uploaded and acked', async () => {
  const h = build({
    transport: new FakeTransport(twoRound('want')),
    media: new FakeMedia([asset('a'), asset('b')]),
  });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ a: 'done', b: 'done' });
  expect(h.transport.uploads.map((upload) => upload.localId).sort()).toEqual(['a', 'b']);
  expect(h.media.hashOpens().sort()).toEqual(['a', 'b']);
  for (const item of h.store.snapshot()) {
    expect(item.assetId).toBeTruthy();
  }
});

test('the upload declares the digest and size recorded at hash time', async () => {
  const files = new Map([['a', { size: 4242, md5: 'deadbeef' }]]);
  const h = build({
    transport: new FakeTransport(twoRound('want')),
    media: new FakeMedia([asset('a')], files),
  });

  await h.engine.run();

  const upload = h.transport.uploads[0] as UploadRequest;
  expect(upload.md5).toBe('deadbeef');
  expect(upload.size).toBe(4242);
  expect(upload.deviceId).toBe(DEVICE);
});

// The payoff of the second check round: content already archived under another
// local id costs a hash, but not a re-upload.
test('a content match in round two skips the upload', async () => {
  const h = build({
    transport: new FakeTransport(twoRound('have')),
    media: new FakeMedia([asset('a')]),
  });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ a: 'done' });
  expect(h.media.hashOpens()).toEqual(['a']);
  expect(h.transport.uploads).toHaveLength(0);
});

test('round one sends the modification time so an edit can be noticed', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    media: new FakeMedia([asset('a', { modifiedAt: 1_700_000_000_000 })]),
  });

  await h.engine.run();

  expect(h.transport.checkCalls[0].items[0].modifiedAt).toBe(
    new Date(1_700_000_000_000).toISOString()
  );
});

// The failure this design exists to prevent: an outage must not be charged to
// the items, or one restart of photod marks a whole library failed.
test('an unreachable server costs no item any attempts', async () => {
  let calls = 0;
  const transport = new FakeTransport((call) => {
    calls += 1;
    if (calls <= 3) throw new SyncError('connection refused', 'unreachable');
    return call.items.map((item) => ({
      localId: item.localId,
      status: 'have' as const,
      assetId: 'asset-x',
    }));
  });
  const h = build({ transport, media: new FakeMedia([asset('a'), asset('b')]) });

  await h.engine.run();

  expect(h.engine.counts.done).toBe(2);
  expect(h.engine.counts.failed).toBe(0);
  for (const item of h.store.snapshot()) {
    expect(item.attempts).toBe(0);
  }
  expect(h.phases).toContain('waiting');
});

test('a 5xx counts against the item and eventually parks it as failed', async () => {
  const transport = new FakeTransport(twoRound('want'), () => {
    throw new SyncError('internal server error', 'server', 500);
  });
  const h = build({ transport, media: new FakeMedia([asset('a')]) });

  await h.engine.run();

  const item = h.store.get('a') as QueueItem;
  expect(item.state).toBe('failed');
  expect(item.attempts).toBe(ITEM_BACKOFF.maxAttempts);
  expect(item.lastError).toContain('internal server error');
  expect(h.transport.uploads).toHaveLength(ITEM_BACKOFF.maxAttempts);
});

// A 422 is this item's problem. It must not stop work on everything else, and it
// must not be reported as the server being unreachable.
test('a rejected item fails without opening the breaker', async () => {
  const transport = new FakeTransport(twoRound('want'), (request) => {
    if (request.localId === 'bad') {
      throw new SyncError('declared md5 does not match received bytes', 'item', 422);
    }
    return { id: `asset-${request.localId}`, sha256: 'sha', duplicate: false };
  });
  const h = build({ transport, media: new FakeMedia([asset('bad'), asset('good')]) });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ bad: 'failed', good: 'done' });
  expect(h.phases).not.toContain('waiting');
  // It did wait, but as an item retry rather than a server outage.
  expect(h.phases).toContain('retrying');
});

test('an item already hashed resumes without hashing again', async () => {
  const h = build({
    transport: new FakeTransport(twoRound('want')),
    media: new FakeMedia([]),
    seed: [queued('a', { state: 'hashed', md5: 'md5-a', size: 100 })],
  });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ a: 'done' });
  expect(h.media.hashOpens()).toEqual([]);
  expect(h.transport.uploads).toHaveLength(1);
});

// What a force-quit mid-upload leaves behind. Phase 0 confirmed iOS aborts the
// transfer, so the item is still `want` and has to be re-sent, not lost.
test('an item left in want after a kill is re-uploaded', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('want')),
    media: new FakeMedia([]),
    seed: [queued('a', { state: 'want', md5: 'md5-a', size: 100 })],
  });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ a: 'done' });
  expect(h.transport.uploads.map((upload) => upload.localId)).toEqual(['a']);
});

test('an item is only marked done after the server acks', async () => {
  const transport = new FakeTransport(twoRound('want'), () => {
    throw new SyncError('dropped', 'item');
  });
  const h = build({ transport, media: new FakeMedia([asset('a')]) });

  await h.engine.run();

  expect(h.store.get('a')?.state).not.toBe('done');
});

test('a temporary copy is released even when the upload fails', async () => {
  const transport = new FakeTransport(twoRound('want'), () => {
    throw new SyncError('dropped', 'item');
  });
  const media = new FakeMedia([asset('live', { kind: 'live_video', parentLocalId: 'still' })]);
  const h = build({ transport, media });

  await h.engine.run();

  // Once for the hash, then once per upload attempt.
  expect(h.media.releases.filter((localId) => localId === 'live').length).toBe(
    h.media.opens.length
  );
});

// If the original changed since it was hashed, sending the stale digest would
// just burn five attempts against a 422. It has to be re-hashed first — and the
// bytes still have to reach the server afterwards.
test('an original that changed since hashing is re-hashed before being sent', async () => {
  const media = new FakeMedia([], new Map([['a', { size: 999, md5: 'md5-a' }]]));
  const h = build({
    transport: new FakeTransport(alwaysRespond('want')),
    media,
    seed: [queued('a', { state: 'want', md5: 'md5-a', size: 100 })],
  });

  await h.engine.run();

  // Never a stale declaration.
  expect(h.transport.uploads.some((upload) => upload.size === 100)).toBe(false);
  expect(h.media.hashOpens()).toEqual(['a']);
  expect(h.transport.uploads.map((upload) => upload.size)).toEqual([999]);
  expect(h.store.get('a')?.state).toBe('done');
});

test('an item the server leaves out of its answer is penalized, not dropped', async () => {
  const transport = new FakeTransport(({ items }) =>
    items.filter((item) => item.localId !== 'ignored').map((item) => ({
      localId: item.localId,
      status: 'have' as const,
      assetId: 'asset-x',
    }))
  );
  const h = build({ transport, media: new FakeMedia([asset('ignored'), asset('answered')]) });

  await h.engine.run();

  const ignored = h.store.get('ignored') as QueueItem;
  expect(ignored.state).toBe('failed');
  expect(ignored.lastError).toContain('left this item out');
  expect(h.store.get('answered')?.state).toBe('done');
});

// The server should never answer "unknown" to a check that carried a digest.
// If it does, asking for the bytes terminates; re-hashing would not.
test('unknown in round two is treated as want rather than looping', async () => {
  const transport = new FakeTransport(({ items }) =>
    items.map((item) => ({ localId: item.localId, status: 'unknown' as const }))
  );
  const h = build({ transport, media: new FakeMedia([asset('a')]) });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ a: 'done' });
  expect(h.transport.uploads).toHaveLength(1);
});

test('checks are split into batches of the configured size', async () => {
  const assets = Array.from({ length: 250 }, (_, index) => asset(`a${index}`));
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    media: new FakeMedia(assets),
    config: { checkBatchSize: 200 },
  });

  await h.engine.run();

  expect(h.transport.checkCalls.map((call) => call.items.length)).toEqual([200, 50]);
  expect(h.engine.counts.done).toBe(250);
});

test('an original that will not open fails only itself', async () => {
  const media = new FakeMedia(
    [asset('broken'), asset('fine')],
    new Map(),
    new Set(['broken'])
  );
  const h = build({ transport: new FakeTransport(twoRound('want')), media });

  await h.engine.run();

  expect(statesOf(h.store)).toEqual({ broken: 'failed', fine: 'done' });
});

test('pause stops the run and leaves the queue resumable', async () => {
  const media = new FakeMedia([asset('a'), asset('b'), asset('c')]);
  const h = build({ transport: new FakeTransport(twoRound('want')), media });

  // Stop as soon as the first hash is announced.
  const engine = h.engine;
  const originalOpen = media.open.bind(media);
  media.open = async (item, opts) => {
    engine.stop();
    return originalOpen(item, opts);
  };

  await engine.run();

  const counts = engine.counts;
  expect(counts.done).toBe(0);
  expect(counts.unknown + counts.hashed).toBe(3);
  expect(h.phases[h.phases.length - 1]).toBe('idle');
});

test('retryFailed returns items to hashed when a digest survives', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    media: new FakeMedia([]),
    seed: [
      queued('withDigest', { state: 'failed', md5: 'md5-x', size: 10, attempts: 5 }),
      queued('withoutDigest', { state: 'failed', attempts: 5 }),
    ],
  });

  const reset = await h.engine.retryFailed();

  expect(reset).toBe(2);
  expect(h.store.get('withDigest')?.state).toBe('hashed');
  expect(h.store.get('withoutDigest')?.state).toBe('pending');
  expect(h.store.get('withDigest')?.attempts).toBe(0);
});

test('re-enumeration does not disturb items already in flight', async () => {
  const store = new MemoryQueueStore([queued('a', { state: 'done', assetId: 'asset-1' })]);
  const added = await store.enqueue([asset('a'), asset('b')]);

  expect(added).toBe(1);
  expect(store.get('a')?.state).toBe('done');
  expect(store.get('b')?.state).toBe('pending');
});

test('a live photo pair is queued and uploaded as two separate items', async () => {
  const h = build({
    transport: new FakeTransport(twoRound('want')),
    media: new FakeMedia([
      asset('still'),
      asset('still#live', { kind: 'live_video', parentLocalId: 'still', filename: 'IMG.MOV' }),
    ]),
  });

  await h.engine.run();

  expect(h.transport.uploads.map((upload) => upload.localId).sort()).toEqual([
    'still',
    'still#live',
  ]);
  expect(h.engine.counts.done).toBe(2);
});

// The server cannot work the pairing out on its own — the two files share
// nothing but a capture time. Without this declaration it archives the clip as
// an item of its own and the gallery draws the same moment twice.
test('a paired video declares which still it belongs to', async () => {
  const h = build({
    transport: new FakeTransport(twoRound('want')),
    media: new FakeMedia([
      asset('still'),
      asset('still#live', { kind: 'live_video', parentLocalId: 'still', filename: 'IMG.MOV' }),
    ]),
  });

  await h.engine.run();

  const byLocalId = new Map(h.transport.uploads.map((upload) => [upload.localId, upload]));
  expect(byLocalId.get('still#live')?.liveParentLocalId).toBe('still');
  expect(byLocalId.get('still')?.liveParentLocalId).toBeFalsy();
});
