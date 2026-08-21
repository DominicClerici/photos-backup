/**
 * The shared-album path through the engine.
 *
 * What is worth testing here is one decision and its consequences: a shared
 * asset never enters the hashing state, because reading it *is* downloading it
 * from iCloud and sending it round the hash-then-check loop would fetch every
 * shared photograph from Apple twice. Everything below is either that rule or
 * something that has to hold because of it — the digest coming from the download,
 * the name coming from the resource, and the library path being left exactly as
 * it was.
 */

import { DEFAULT_ENGINE_CONFIG, SyncEngine, type EngineConfig } from '../engine';
import { MemoryQueueStore } from '../memoryStore';
import type { Progress, QueueItem } from '../types';
import {
  alwaysRespond,
  asset,
  FakeMedia,
  FakeTransport,
  queued,
  TestClock,
  twoRound,
  type FakeFile,
} from './fakes';

const DEVICE = 'ios-test';

function build(options: {
  transport: FakeTransport;
  media?: FakeMedia;
  seed?: QueueItem[];
  config?: Partial<EngineConfig>;
}) {
  const store = new MemoryQueueStore(options.seed ?? []);
  const media = options.media ?? new FakeMedia();
  const engine = new SyncEngine(
    {
      store,
      media,
      transport: options.transport,
      clock: new TestClock(),
      onProgress: (_: Progress) => {},
    },
    { ...DEFAULT_ENGINE_CONFIG, deviceId: DEVICE, ...options.config }
  );
  return { engine, store, media, transport: options.transport };
}

function queuedShared(localId: string): QueueItem {
  return queued(localId, { source: 'shared' });
}

function shared(localId: string, file?: FakeFile) {
  const media = new FakeMedia(
    [asset(localId, { source: 'shared' })],
    file ? new Map([[localId, file]]) : new Map()
  );
  return media;
}

test('a shared asset is fetched once and uploaded, never hashed first', async () => {
  const h = build({
    // The archive has never seen this local id and no digest was offered, which
    // is the answer that would send a library original away to be hashed.
    transport: new FakeTransport(alwaysRespond('unknown')),
    media: shared('ph://shared-1'),
  });

  await h.engine.run();

  expect(h.transport.uploads.map((u) => u.localId)).toEqual(['ph://shared-1']);
  // One open, not two: the download that produced the bytes is the one that
  // produced the digest.
  expect(h.media.opens).toEqual([{ localId: 'ph://shared-1', hash: true }]);
  expect(h.engine.counts.done).toBe(1);
});

test('the digest it declares is the one the download produced', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown')),
    media: shared('ph://shared-1', { size: 4096, md5: 'from-icloud' }),
  });

  await h.engine.run();

  expect(h.transport.uploads[0].md5).toBe('from-icloud');
  expect(h.transport.uploads[0].size).toBe(4096);
});

// Apple re-encodes what goes into a shared album, so PhotoKit's name for the
// asset and the name of the file it actually hands over can disagree about the
// format — and the server trusts a recognised extension over the bytes.
test('the archive is told what the resource is called, not what the asset is called', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown')),
    media: shared('ph://shared-1', { size: 10, md5: 'm', filename: 'IMG_0042.JPG' }),
  });

  await h.engine.run();

  expect(h.transport.uploads[0].filename).toBe('IMG_0042.JPG');
});

// So that a later reopenDone() can settle the item against the archive by
// content instead of pulling it out of iCloud all over again.
test('what was sent is written down once the archive has acked it', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown')),
    media: shared('ph://shared-1', { size: 4096, md5: 'from-icloud' }),
  });

  await h.engine.run();

  const [item] = h.store.snapshot();
  expect(item.state).toBe('done');
  expect(item.md5).toBe('from-icloud');
  expect(item.size).toBe(4096);
});

// The archive already holding this local id is the cheap answer, and it has to
// stay cheap: a second run must not go back to iCloud for anything.
test('an asset the archive already has is never fetched at all', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    media: shared('ph://shared-1'),
  });

  await h.engine.run();

  expect(h.media.opens).toEqual([]);
  expect(h.transport.uploads).toEqual([]);
  expect(h.engine.counts.done).toBe(1);
});

test('a library original still hashes before the archive is asked for it', async () => {
  const h = build({
    transport: new FakeTransport(twoRound('want')),
    media: new FakeMedia([asset('ph://library-1')]),
  });

  await h.engine.run();

  // Round one asks without a digest, round two asks with one: the whole point of
  // the hashing state, and the thing the shared path is allowed to skip.
  expect(h.transport.checkCalls).toHaveLength(2);
  expect(h.transport.checkCalls[0].items[0].md5).toBeUndefined();
  expect(h.transport.checkCalls[1].items[0].md5).toBe('md5-ph://library-1');
});

test('a shared and a library asset in one run each take their own route', async () => {
  const media = new FakeMedia([asset('ph://library-1'), asset('ph://shared-1', { source: 'shared' })]);
  const h = build({ transport: new FakeTransport(twoRound('want')), media });

  await h.engine.run();

  expect(h.media.hashOpens()).toContain('ph://shared-1');
  expect(h.transport.uploads.map((u) => u.localId).sort()).toEqual([
    'ph://library-1',
    'ph://shared-1',
  ]);
  // The library original was opened twice — once to hash, once to send — and the
  // shared one only once.
  expect(h.media.opens.filter((o) => o.localId === 'ph://shared-1')).toHaveLength(1);
  expect(h.media.opens.filter((o) => o.localId === 'ph://library-1')).toHaveLength(2);
});

// The checkbox has to mean something for the photographs already queued out of
// an album, not only for the next ones.
test('unticking an album drops what it had queued and not finished', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown')),
    // An enumeration that offers nothing shared is what an unticked album looks
    // like from in here.
    media: new FakeMedia([]),
    seed: [{ ...queuedShared('ph://shared-1'), state: 'want' }],
  });

  await h.engine.run();

  expect(h.store.snapshot()).toEqual([]);
  expect(h.transport.uploads).toEqual([]);
});

// Dropping these would be safe and wasteful: the row is the record that the
// archive holds the photograph, and re-ticking the album settles it in one check
// rather than fetching every asset out of iCloud again.
test('what is already archived is kept even when the album is unticked', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown')),
    media: new FakeMedia([]),
    seed: [{ ...queuedShared('ph://shared-1'), state: 'done', assetId: 'asset-1' }],
  });

  await h.engine.run();

  expect(h.store.snapshot().map((item) => item.localId)).toEqual(['ph://shared-1']);
});

// The camera roll is not additive in the same way and must not be touched by
// any of this: a photograph the enumerator skipped is still on the phone.
test('a library item is never dropped for being absent from one enumeration', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown')),
    media: new FakeMedia([]),
    seed: [{ ...queued('ph://library-1'), state: 'want', md5: 'm', size: 1 }],
  });

  await h.engine.run();

  expect(h.store.snapshot().map((item) => item.localId)).toEqual(['ph://library-1']);
});

// A failed download is an ordinary item failure: it backs off, retries, and
// eventually parks itself without taking the run down with it.
test('an asset iCloud will not hand over fails on its own', async () => {
  const media = new FakeMedia(
    [asset('ph://shared-1', { source: 'shared' }), asset('ph://shared-2', { source: 'shared' })],
    new Map(),
    new Set(['ph://shared-1'])
  );
  const h = build({ transport: new FakeTransport(alwaysRespond('unknown')), media });

  await h.engine.run();

  expect(h.engine.counts.failed).toBe(1);
  expect(h.engine.counts.done).toBe(1);
  expect(h.transport.uploads.map((u) => u.localId)).toEqual(['ph://shared-2']);
});

// The downloaded copy is a file this app made, and the only thing that deletes
// it is the release the engine runs when it is finished with it.
test('the downloaded copy is released whether the upload worked or not', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('unknown'), () => {
      throw new Error('the archive refused it');
    }),
    media: shared('ph://shared-1'),
  });

  await h.engine.run();

  expect(h.media.releases).toContain('ph://shared-1');
});

// The repair path for photographs archived before the phone knew how to record
// an album title. `done` is the one state nothing else can leave, and the origin
// only ever arrives on a fresh row — so the fix is to delete and re-enumerate.
// It costs no bytes: the archive answers `have` and describes on the way past.
test('forgetting shared items re-offers them with what this build knows', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    media: shared('ph://shared-1'),
    // Archived by a build that could not name the album it came out of.
    seed: [{ ...queuedShared('ph://shared-1'), state: 'done', assetId: 'asset-1' }],
  });
  h.media.facts_.set('ph://shared-1', {
    favorite: false,
    subtypes: [],
    albums: ['GTI-IS-ST'],
    location: null,
    photoKit: null,
  });

  expect(await h.store.forgetShared()).toBe(1);
  await h.engine.run();

  expect(h.transport.uploads).toEqual([]);
  expect(h.transport.described.map((d) => d.facts.albums)).toEqual([['GTI-IS-ST']]);
  expect(h.engine.counts.done).toBe(1);
});

// Deleting more than was asked for would mean re-uploading a camera roll.
test('forgetting shared items leaves the library alone', async () => {
  const h = build({
    transport: new FakeTransport(alwaysRespond('have')),
    seed: [
      { ...queuedShared('ph://shared-1'), state: 'done' },
      { ...queued('ph://library-1'), state: 'done' },
    ],
  });

  expect(await h.store.forgetShared()).toBe(1);
  expect(h.store.snapshot().map((item) => item.localId)).toEqual(['ph://library-1']);
});
