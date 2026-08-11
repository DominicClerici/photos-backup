import { DEFAULT_ENGINE_CONFIG, SyncEngine } from '../engine';
import { MemoryQueueStore } from '../memoryStore';
import { isUnauthorized, SyncError, type Progress, type QueueItem } from '../types';
import { FakeMedia, FakeTransport, queued, TestClock } from './fakes';

const DEVICE = 'device-uuid';

/**
 * A revoked token, or a pairing that no longer matches.
 *
 * The whole reason this failure has its own kind: it is neither the server's
 * fault nor any item's, so neither the circuit breaker nor an item's attempt
 * count should absorb it. What must happen instead is that the run stops and says
 * so, because retrying forty thousand items against a token that will never work
 * again would mark the entire library failed and then need a manual retry after
 * the pairing was fixed.
 */
function refuse(status: number, message = 'this device has been unpaired'): never {
  throw new SyncError(`sync/check returned ${status}: ${message}`, 'unauthorized', status);
}

function build(seed: QueueItem[], transport: FakeTransport) {
  const store = new MemoryQueueStore(seed);
  const clock = new TestClock();
  const phases: Progress['phase'][] = [];
  const logs: string[] = [];

  const engine = new SyncEngine(
    {
      store,
      media: new FakeMedia(),
      transport,
      clock,
      onProgress: (progress) => phases.push(progress.phase),
      onLog: (line) => logs.push(line),
    },
    { ...DEFAULT_ENGINE_CONFIG, deviceId: DEVICE }
  );
  return { engine, store, clock, phases, logs };
}

it('stops the run rather than blaming the items', async () => {
  const seed = [queued('a'), queued('b'), queued('c')];
  const { engine, store, logs } = build(
    seed,
    new FakeTransport(() => refuse(401))
  );

  await expect(engine.run()).rejects.toThrow(/unpaired/);

  const counts = await store.counts();
  expect(counts.failed).toBe(0);
  expect(counts.pending).toBe(3);
  for (const item of await store.due('pending', 10, 0)) {
    // Untouched: no attempt charged, and nothing waiting out a backoff it did
    // not earn.
    expect(item.attempts).toBe(0);
    expect(item.nextAttemptAt).toBe(0);
  }
  expect(logs.some((line) => line.includes('refused this device'))).toBe(true);
});

it('does not hold the circuit breaker open, because the server is fine', async () => {
  const { engine, phases } = build([queued('a')], new FakeTransport(() => refuse(401)));

  await expect(engine.run()).rejects.toThrow();

  // 'waiting' is the breaker's phase. Reaching it here would frame a pairing
  // problem as an outage and sit through a backoff to arrive at the same answer.
  expect(phases).not.toContain('waiting');
  expect(phases).not.toContain('retrying');
});

it('gives up on the first refusal instead of working through the batch', async () => {
  let calls = 0;
  const transport = new FakeTransport(() => {
    calls += 1;
    refuse(401);
  });
  const { engine } = build([queued('a'), queued('b'), queued('c')], transport);

  await expect(engine.run()).rejects.toThrow();

  expect(calls).toBe(1);
});

it('surfaces the failure as unauthorized so the caller can offer to pair', async () => {
  const { engine } = build([queued('a')], new FakeTransport(() => refuse(403)));

  const error = await engine.run().catch((e: unknown) => e);

  expect(isUnauthorized(error)).toBe(true);
});

// 426 is photod's answer on its read-only plaintext listener. It means the
// address in use is the gallery's, not the upload path's, so no amount of
// retrying will get an upload accepted — same handling, different cause.
it('treats a plaintext listener the same way', async () => {
  const { engine, store } = build(
    [queued('a')],
    new FakeTransport(() => refuse(426, 'this endpoint is only served over HTTPS'))
  );

  await expect(engine.run()).rejects.toThrow(/HTTPS/);
  expect((await store.counts()).failed).toBe(0);
});

// An upload refused mid-flight has to behave the same as a refused check, and it
// still has to release the temporary copy it opened — a Live Photo's extracted
// video is a real file on disk.
it('releases the original it opened when an upload is refused', async () => {
  // FakeMedia reports 100 bytes for an unknown file, so the queued size has to
  // agree or the engine re-hashes instead of uploading.
  const store = new MemoryQueueStore([queued('a', { state: 'want', md5: 'abc', size: 100 })]);
  const media = new FakeMedia();
  const engine = new SyncEngine(
    {
      store,
      media,
      transport: new FakeTransport(
        () => [],
        () => {
          throw new SyncError('upload returned 401: unpaired', 'unauthorized', 401);
        }
      ),
      clock: new TestClock(),
    },
    { ...DEFAULT_ENGINE_CONFIG, deviceId: DEVICE }
  );

  await expect(engine.run()).rejects.toThrow(/unpaired/);

  expect(media.releases).toEqual(['a']);
  expect((await store.counts()).failed).toBe(0);
  // Still `want`: the bytes were never accepted, so the item is exactly where it
  // was and a re-pairing picks it straight back up.
  expect((await store.counts()).want).toBe(1);
});
