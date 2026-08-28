/**
 * The shared-album download at its real boundary, rather than at the fake one.
 *
 * Everything else that exercises this path substitutes FakeMedia for the whole
 * of media.ts — which is what shared.test.ts wants, because what it is testing
 * is the engine's route through the queue. The cost of that substitution is that
 * openShared() itself, the one function in the file that talks to iCloud, was
 * covered by nothing: its gate was tested with a fake download that threw, its
 * caller was tested with a fake media source that never used a gate, and the
 * seam between them — where the native module reports a refusal by resolving
 * rather than throwing — was where a real bug lived, pacing a throttled phone at
 * full speed while every test passed.
 *
 * So this one mocks the three modules that need a device and loads the real
 * thing. The header on facts.ts says media.ts "cannot be loaded under jest";
 * that was true of a bare import and is not true of a mocked one.
 */

jest.mock('expo-file-system', () => {
  class Directory {
    uri: string;
    exists = true;
    constructor(...parts: string[]) {
      this.uri = parts.join('/');
    }
    create(): void {}
    list(): unknown[] {
      return [];
    }
  }

  class File {
    uri: string;
    exists = true;
    size = 0;
    constructor(parent: { uri: string } | string, name?: string) {
      this.uri = typeof parent === 'string' ? parent : `${parent.uri}/${name}`;
    }
    delete(): void {}
  }

  return { Directory, File, Paths: { cache: 'file:///cache' } };
});

jest.mock('expo-media-library', () => ({
  Asset: class {},
  AssetField: {},
  MediaSubtype: {},
  MediaType: {},
  Query: class {},
}));

jest.mock('../../../modules/photo-facts', () => ({
  photoKitDownloadSharedResource: jest.fn(),
  photoKitOnSharedFetchProgress: jest.fn(() => () => {}),
  photoKitEnumerate: jest.fn(),
  photoKitFacts: jest.fn(),
  photoKitMd5: jest.fn(),
  photoKitSharedAlbums: jest.fn(),
}));

import {
  photoKitDownloadSharedResource,
  photoKitOnSharedFetchProgress,
  type SharedFetchProgress,
} from '../../../modules/photo-facts';
import { SharedFetchGate } from '../../sharedalbums/gate';
import { CALM_GAP_MS } from '../../sharedalbums/run';
import { PhotoKitMediaSource } from '../media';
import type { OpenProgress } from '../types';
import { queued, TestClock } from './fakes';

const download = photoKitDownloadSharedResource as jest.MockedFunction<
  typeof photoKitDownloadSharedResource
>;
const watch = photoKitOnSharedFetchProgress as jest.MockedFunction<
  typeof photoKitOnSharedFetchProgress
>;

beforeEach(() => {
  jest.clearAllMocks();
});

/** A shared still, as the queue holds one waiting to be fetched. */
function sharedItem(localId = 'ph://shared-1') {
  return queued(localId, { source: 'shared' });
}

function refusal(message = 'the network connection was lost') {
  return {
    ok: false as const,
    failure: { domain: 'NSURLErrorDomain', code: -1005, message, bytes: 0, elapsedMs: 12 },
  };
}

function handedOver() {
  return {
    ok: true as const,
    download: {
      path: '/cache/sharedalbums/x.mov',
      bytes: 1_024,
      md5: 'abc',
      elapsedMs: 40,
      uniformTypeIdentifier: 'public.jpeg',
      originalFilename: 'IMG_0001.HEIC',
      resourceType: 'photo',
    },
  };
}

function source(gate: SharedFetchGate) {
  return new PhotoKitMediaSource({ gate });
}

test('a download that worked leaves the gate at its calm pace', async () => {
  const gate = new SharedFetchGate(new TestClock());
  download.mockResolvedValue(handedOver());

  await source(gate).open(sharedItem(), { hash: true });

  expect(gate.gapMs()).toBe(CALM_GAP_MS);
});

// The bug this file was written for. The native module reports a refusal by
// resolving with ok:false rather than by rejecting — deliberately, so the caller
// gets Apple's domain and code intact — and the classification of that answer
// has to happen inside the gate. Outside it, every refusal looked like a
// success: strain stayed at zero, the pause stayed at 150ms, and a phone iCloud
// was actively pushing back on went on asking as fast as it could.
test('a refusal iCloud resolved rather than threw still stretches the pause', async () => {
  const gate = new SharedFetchGate(new TestClock());
  download.mockResolvedValue(refusal());

  await expect(source(gate).open(sharedItem(), { hash: true })).rejects.toThrow(
    /would not hand over/
  );

  expect(gate.gapMs()).toBeGreaterThan(CALM_GAP_MS);
});

test('the failure still reaches the caller with what Apple said about it', async () => {
  const gate = new SharedFetchGate(new TestClock());
  download.mockResolvedValue(refusal('the network connection was lost'));

  await expect(source(gate).open(sharedItem(), { hash: true })).rejects.toThrow(
    /NSURLErrorDomain -1005.*the network connection was lost/
  );
});

// Repeated refusals are the case the pacing exists for, and the gap has to keep
// climbing across items rather than resetting with each one.
test('the pause keeps climbing while the refusals keep coming', async () => {
  const gate = new SharedFetchGate(new TestClock());
  download.mockResolvedValue(refusal());
  const media = source(gate);

  await expect(media.open(sharedItem('a'), { hash: true })).rejects.toThrow();
  const afterOne = gate.gapMs();
  await expect(media.open(sharedItem('b'), { hash: true })).rejects.toThrow();

  expect(gate.gapMs()).toBeGreaterThan(afterOne);
});

// A build too old to carry the downloader answers null, which is not iCloud
// saying no — but it is still a failed download, and the gate's own header is
// explicit that it would rather pause for a failure that was nobody's fault than
// have to be told whose fault each one was.
test('a build that cannot download at all fails the item', async () => {
  const gate = new SharedFetchGate(new TestClock());
  download.mockResolvedValue(null);

  await expect(source(gate).open(sharedItem(), { hash: true })).rejects.toThrow(
    /this build cannot download shared assets/
  );
});

/**
 * The progress the backup shows while a shared asset comes down.
 *
 * Worth its own tests for one reason that is easy to get wrong and invisible
 * when it is: a Live Photo's video half is queued under an identifier of our own
 * and fetched under its parent's, so the listener has to filter on the second or
 * the motion half of every shared Live Photo downloads in silence.
 */
function watching(): { emit: (progress: SharedFetchProgress) => void; removed: () => number } {
  let removals = 0;
  const listeners: ((progress: SharedFetchProgress) => void)[] = [];

  watch.mockImplementation((listener) => {
    listeners.push(listener);
    return () => {
      removals += 1;
    };
  });

  return {
    emit: (progress) => listeners.forEach((listener) => listener(progress)),
    removed: () => removals,
  };
}

test('what iCloud reports about the asset in flight reaches the caller', async () => {
  const events = watching();
  const seen: OpenProgress[] = [];
  download.mockImplementation(async () => {
    events.emit({ localId: 'ph://shared-1', bytes: 2_048, fraction: 0.5 });
    return handedOver();
  });

  await source(new SharedFetchGate(new TestClock())).open(sharedItem(), {
    hash: true,
    onProgress: (progress) => seen.push(progress),
  });

  expect(seen).toEqual([{ bytes: 2_048, fraction: 0.5 }]);
});

// Events from the fetch before this one can still be in flight. Reporting them
// against this asset would run the label backwards.
test('what iCloud reports about some other asset is ignored', async () => {
  const events = watching();
  const seen: OpenProgress[] = [];
  download.mockImplementation(async () => {
    events.emit({ localId: 'ph://somebody-else', bytes: 999, fraction: 0.9 });
    return handedOver();
  });

  await source(new SharedFetchGate(new TestClock())).open(sharedItem(), {
    hash: true,
    onProgress: (progress) => seen.push(progress),
  });

  expect(seen).toEqual([]);
});

test("a shared Live Photo's video half is reported under its parent's id", async () => {
  const events = watching();
  const seen: OpenProgress[] = [];
  download.mockImplementation(async () => {
    events.emit({ localId: 'ph://parent', bytes: 4_096, fraction: 0.2 });
    return handedOver();
  });

  const video = queued('ph://parent#live', {
    source: 'shared',
    kind: 'live_video',
    parentLocalId: 'ph://parent',
  });
  await source(new SharedFetchGate(new TestClock())).open(video, {
    hash: true,
    onProgress: (progress) => seen.push(progress),
  });

  expect(seen).toEqual([{ bytes: 4_096, fraction: 0.2 }]);
});

// A listener per download that outlived it would have every later fetch
// reporting itself to every earlier one's caller.
test('the listener is taken down with the download, failed or not', async () => {
  const events = watching();
  const media = source(new SharedFetchGate(new TestClock()));
  const noop = () => {};

  download.mockResolvedValueOnce(handedOver());
  await media.open(sharedItem(), { hash: true, onProgress: noop });
  download.mockResolvedValueOnce(refusal());
  await expect(media.open(sharedItem(), { hash: true, onProgress: noop })).rejects.toThrow();

  expect(events.removed()).toBe(2);
});

test('a caller that wants no progress subscribes to none', async () => {
  watching();
  download.mockResolvedValue(handedOver());

  await source(new SharedFetchGate(new TestClock())).open(sharedItem(), { hash: true });

  expect(watch).not.toHaveBeenCalled();
});
