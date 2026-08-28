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

import { photoKitDownloadSharedResource } from '../../../modules/photo-facts';
import { SharedFetchGate } from '../../sharedalbums/gate';
import { CALM_GAP_MS } from '../../sharedalbums/run';
import { PhotoKitMediaSource } from '../media';
import { queued, TestClock } from './fakes';

const download = photoKitDownloadSharedResource as jest.MockedFunction<
  typeof photoKitDownloadSharedResource
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
