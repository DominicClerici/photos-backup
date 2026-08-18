import { CHUNK_THRESHOLD } from '../chunkPlan';
import { DEFAULT_ENGINE_CONFIG, SyncEngine } from '../engine';
import { MemoryQueueStore } from '../memoryStore';
import { systemClock, type Progress, type QueueItem } from '../types';
import { alwaysRespond, FakeMedia, FakeTransport, queued, TestClock } from './fakes';

const DEVICE = 'ios-test';

/** A queue item already cleared for upload, at a chosen size. */
function wanted(localId: string, size: number): QueueItem {
  return queued(localId, { state: 'want', size, md5: `md5-${localId}` });
}

function run(seed: QueueItem[]) {
  const store = new MemoryQueueStore(seed);
  const transport = new FakeTransport(alwaysRespond('want'));
  const files = new Map(seed.map((item) => [item.localId, { size: item.size ?? 0, md5: item.md5! }]));
  const media = new FakeMedia([], files);
  const progress: Progress[] = [];

  const engine = new SyncEngine(
    {
      store,
      media,
      transport,
      clock: new TestClock(),
      onProgress: (p) => progress.push(p),
    },
    { ...DEFAULT_ENGINE_CONFIG, deviceId: DEVICE }
  );

  return { engine, store, transport, progress };
}

// The size threshold is the whole routing decision, and getting it wrong in
// either direction is expensive: a small photo paying for three round trips, or
// a 550MB video that restarts from zero every time Wi-Fi blinks.
test('an original at the threshold takes the resumable path', async () => {
  const { engine, transport } = run([wanted('big', CHUNK_THRESHOLD)]);

  await engine.run();

  expect(transport.resumable).toEqual(['big']);
});

test('an original below the threshold takes the single-shot path', async () => {
  const { engine, transport } = run([wanted('small', CHUNK_THRESHOLD - 1)]);

  await engine.run();

  expect(transport.resumable).toEqual([]);
  expect(transport.uploads.map((u) => u.localId)).toEqual(['small']);
});

test('a mixed batch routes each original by its own size', async () => {
  const { engine, transport, store } = run([
    wanted('photo-1', 3_000_000),
    wanted('video', 200_000_000),
    wanted('photo-2', 4_000_000),
  ]);

  await engine.run();

  expect(transport.resumable).toEqual(['video']);
  expect(transport.uploads).toHaveLength(3);
  expect((await store.counts()).done).toBe(3);
});

// A minutes-long upload that says nothing is indistinguishable from a stuck one.
test('a resumable upload reports its progress as a percentage', async () => {
  const { engine, progress } = run([wanted('video', 200_000_000)]);

  await engine.run();

  const labels = progress.filter((p) => p.phase === 'uploading').map((p) => p.activity);
  expect(labels.some((label) => /\d+%/.test(label))).toBe(true);
  expect(labels).toContain('Uploading video.HEIC — 100%');
});

// Both paths end the same way: the item is only done once the server has acked.
test('a resumable upload is only marked done after the commit', async () => {
  const { engine, store } = run([wanted('video', 200_000_000)]);

  await engine.run();

  const item = await store.get('video');
  expect(item?.state).toBe('done');
  expect(item?.assetId).toBeTruthy();
});

test('the default config no longer caps the library at the test fixture size', () => {
  // Phase 2 shipped 110, the size of the test fixture. A real backfill is the
  // whole camera roll, and a cap would leave most of it silently unarchived.
  expect(DEFAULT_ENGINE_CONFIG.maxItems).toBe(0);
});

test('the clock used in production is the real one', () => {
  expect(typeof systemClock.now()).toBe('number');
});
