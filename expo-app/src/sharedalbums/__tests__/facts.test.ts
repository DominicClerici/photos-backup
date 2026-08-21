import type { PhotoKitFacts, SharedContributor } from '../../../modules/photo-facts';
import type { SharedOrigin } from '../../sync/types';
import { albumTitles, sharedFacts } from '../facts';

function contributor(overrides: Partial<SharedContributor> = {}): SharedContributor {
  return {
    firstName: 'Andres',
    lastName: 'Castillo',
    email: null,
    personId: null,
    displayName: 'Andres Castillo',
    ...overrides,
  };
}

function photoKit(overrides: Partial<PhotoKitFacts> = {}): PhotoKitFacts {
  return {
    localId: 'ph://asset',
    hidden: false,
    favorite: false,
    mediaType: { value: 1, name: 'image' },
    mediaSubtypes: { value: 0, names: [] },
    sourceType: { value: 2, names: ['typeCloudShared'] },
    playbackStyle: { value: 1, name: 'image' },
    burstIdentifier: null,
    burstSelectionTypes: { value: 0, names: [] },
    representsBurst: false,
    pixelWidth: 2049,
    pixelHeight: 1537,
    durationSeconds: 0,
    createdAt: null,
    modifiedAt: null,
    hasAdjustments: false,
    originalFilename: 'IMG_6822.HEIC',
    resources: [],
    location: null,
    sharedAlbums: [],
    contributor: null,
    ...overrides,
  };
}

function origin(overrides: Partial<SharedOrigin> = {}): SharedOrigin {
  return { albums: ['GTI-IS-ST'], contributor: contributor(), ...overrides };
}

describe('sharedFacts', () => {
  // PhotoKit answers neither question when asked about one asset, which is how
  // fifty-five photographs were filed under no album with nobody's name on them.
  it('puts back the album and the contributor PhotoKit will not name', () => {
    const merged = sharedFacts(photoKit(), origin());

    expect(merged?.sharedAlbums).toEqual(['GTI-IS-ST']);
    expect(merged?.contributor?.displayName).toBe('Andres Castillo');
  });

  it('prefers what the phone answered, so an iOS that starts answering wins', () => {
    const live = photoKit({
      sharedAlbums: ['From PhotoKit'],
      contributor: contributor({ displayName: 'Someone Else' }),
    });
    const merged = sharedFacts(live, origin());

    expect(merged?.contributor?.displayName).toBe('Someone Else');
    expect(merged?.sharedAlbums).toEqual(['From PhotoKit', 'GTI-IS-ST']);
  });

  it('leaves a camera roll asset exactly as PhotoKit described it', () => {
    const facts = photoKit();
    expect(sharedFacts(facts, null)).toBe(facts);
  });

  it('has nothing to add to a build with no native module', () => {
    expect(sharedFacts(null, origin())).toBeNull();
  });

  // A shared album whose contributor could not be read is still a shared album
  // worth filing, so the two halves fail independently.
  it('files the album even where nobody could be named', () => {
    const merged = sharedFacts(photoKit(), origin({ contributor: null }));

    expect(merged?.sharedAlbums).toEqual(['GTI-IS-ST']);
    expect(merged?.contributor).toBeNull();
  });
});

describe('albumTitles', () => {
  it('merges what each side knows, once each', () => {
    expect(albumTitles(['Instagram'], ['GTI-IS-ST'])).toEqual(['Instagram', 'GTI-IS-ST']);
    expect(albumTitles(['Iceland'], ['Iceland'])).toEqual(['Iceland']);
  });

  it('drops a title that is not one', () => {
    expect(albumTitles(['', '  '], ['  GTI-IS-ST  '])).toEqual(['GTI-IS-ST']);
  });
});
