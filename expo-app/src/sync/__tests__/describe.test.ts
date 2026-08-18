import { DEFAULT_ENGINE_CONFIG, SyncEngine, type EngineConfig } from '../engine';
import { MemoryQueueStore } from '../memoryStore';
import type { Progress, QueueItem } from '../types';
import { alwaysRespond, asset, FakeMedia, FakeTransport, facts, photoKit, TestClock } from './fakes';

const DEVICE = 'ios-test';

function build(options: {
  transport: FakeTransport;
  media?: FakeMedia;
  seed?: QueueItem[];
  config?: Partial<EngineConfig>;
}) {
  const store = new MemoryQueueStore(options.seed ?? []);
  const media = options.media ?? new FakeMedia();
  const logs: string[] = [];

  const engine = new SyncEngine(
    {
      store,
      media,
      transport: options.transport,
      clock: new TestClock(),
      onProgress: (_: Progress) => {},
      onLog: (line: string) => logs.push(line),
    },
    { ...DEFAULT_ENGINE_CONFIG, deviceId: DEVICE, ...options.config }
  );

  return { engine, store, media, transport: options.transport, logs };
}

// A heart, an album and "this is a screenshot" are decisions a person made.
// Nothing in the bytes records them, so an upload that does not carry them
// loses them the day the photo leaves the phone.
test('an uploaded asset is described with what the library knows', async () => {
  const media = new FakeMedia([asset('a')]);
  media.facts_.set('a', facts({ favorite: true, subtypes: ['screenshot'], albums: ['Iceland'] }));

  const h = build({ transport: new FakeTransport(alwaysRespond('want')), media });
  await h.engine.run();

  expect(h.transport.described).toHaveLength(1);
  expect(h.transport.described[0].facts.favorite).toBe(true);
  expect(h.transport.described[0].facts.albums).toEqual(['Iceland']);
});

// The case that matters most for a library that is already backed up: the
// server has the bytes, nothing will ever be uploaded again, and this is the
// only moment the hearts and albums can still be collected.
test('an asset the archive already holds is described too', async () => {
  const media = new FakeMedia([asset('a')]);
  media.facts_.set('a', facts({ favorite: true }));

  const h = build({ transport: new FakeTransport(alwaysRespond('have')), media });
  await h.engine.run();

  expect(h.transport.uploads).toHaveLength(0);
  expect(h.transport.described.map((call) => call.assetId)).toEqual(['asset-for-a']);
});

// One request per asset that has something in it, not per asset. Without the
// native module that is still the old four questions, which is what a JS reload
// against an older dev client gets.
test('an asset with nothing to say is not described', async () => {
  const media = new FakeMedia([asset('a')]);
  media.facts_.set('a', facts());

  const h = build({ transport: new FakeTransport(alwaysRespond('have')), media });
  await h.engine.run();

  expect(h.transport.described).toHaveLength(0);
});

// The Hidden album is the fact the native module was built for, and it is
// exactly the kind of asset the old "is this interesting?" check threw away:
// no heart, no album, no subtype, no location, and a decision a person made
// that nothing but the phone records.
test('an asset whose only fact is one the native module found is still described', async () => {
  const media = new FakeMedia([asset('a')]);
  media.facts_.set('a', facts({ photoKit: photoKit({ hidden: true }) }));

  const h = build({ transport: new FakeTransport(alwaysRespond('have')), media });
  await h.engine.run();

  expect(h.transport.described).toHaveLength(1);
  expect(h.transport.described[0].facts.photoKit?.hidden).toBe(true);
});

// A bundle that knows about PhotoFacts running on a binary that does not carry
// it is the ordinary state of development here, so it has to describe an asset
// with exactly what it did before the module existed.
test('a build without the native module describes what the library alone knows', async () => {
  const media = new FakeMedia([asset('a')]);
  media.facts_.set('a', facts({ favorite: true, albums: ['Iceland'] }));

  const h = build({ transport: new FakeTransport(alwaysRespond('have')), media });
  await h.engine.run();

  expect(h.transport.described).toHaveLength(1);
  expect(h.transport.described[0].facts).toEqual({
    favorite: true,
    subtypes: [],
    albums: ['Iceland'],
    location: null,
    photoKit: null,
  });
});

// The bytes are archived and acked before this runs. Charging the item for it
// would re-upload an original over a failed album lookup.
test('a failed description never fails the item', async () => {
  const media = new FakeMedia([asset('a')]);
  media.facts_.set('a', facts({ favorite: true }));

  const transport = new FakeTransport(alwaysRespond('want'));
  transport.describeFails = true;

  const h = build({ transport, media });
  await h.engine.run();

  expect(h.engine.counts.done).toBe(1);
  expect(h.engine.counts.failed).toBe(0);
  expect(h.logs.some((line) => line.includes('could not record'))).toBe(true);
});
