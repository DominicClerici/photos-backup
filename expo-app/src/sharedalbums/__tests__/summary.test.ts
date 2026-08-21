import type { SharedAlbum, SharedAsset } from '../../../modules/photo-facts';
import {
  assetsOf,
  formatDuration,
  pickSample,
  summarize,
  throughputMbPerSecond,
  VIDEO_SECONDS_CAP,
} from '../summary';

let counter = 0;

function asset(overrides: Partial<SharedAsset> = {}): SharedAsset {
  counter += 1;
  return {
    localId: `ph://asset-${counter}`,
    kind: 'still',
    filename: `IMG_${counter}.HEIC`,
    createdAt: 1_700_000_000_000 + counter,
    modifiedAt: null,
    isLive: false,
    pixelWidth: 2048,
    pixelHeight: 1536,
    durationSeconds: 0,
    sourceTypes: { value: 2, names: ['typeCloudShared'] },
    resourceTypes: ['photo'],
    ...overrides,
  };
}

function album(assets: SharedAsset[], title = 'Iceland'): SharedAlbum {
  return { localId: `ph://album-${title}`, title, startDate: null, endDate: null, assets };
}

describe('summarize', () => {
  it('counts an asset once no matter how many albums hold it', () => {
    const shared = asset();
    const survey = summarize([album([shared], 'A'), album([shared], 'B')]);

    expect(survey.assets).toBe(1);
    expect(survey.inMultipleAlbums).toBe(1);
  });

  it('still reports each album at the size the Photos app shows it', () => {
    const shared = asset();
    const survey = summarize([album([shared, asset()], 'A'), album([shared], 'B')]);

    expect(survey.albums.map((a) => a.assets)).toEqual([2, 1]);
    expect(survey.assets).toBe(2);
  });

  it('separates a ceiling from a coincidence', () => {
    const survey = summarize([
      album([
        asset({ pixelWidth: 2048, pixelHeight: 1536 }),
        asset({ pixelWidth: 1536, pixelHeight: 2048 }),
        asset({ pixelWidth: 1024, pixelHeight: 768 }),
      ]),
    ]);

    expect(survey.still.maxLongEdge).toBe(2048);
    expect(survey.still.atMax).toBe(2);
    expect(survey.still.overCap).toBe(0);
  });

  it('flags anything that got through above the documented cap', () => {
    const survey = summarize([album([asset({ pixelWidth: 4032, pixelHeight: 3024 })])]);

    expect(survey.still.overCap).toBe(1);
    expect(survey.still.maxLongEdge).toBe(4032);
  });

  it('keeps stills and videos in separate columns', () => {
    const survey = summarize([
      album([
        asset({ pixelWidth: 2048, pixelHeight: 1536 }),
        asset({ kind: 'video', pixelWidth: 1280, pixelHeight: 720, durationSeconds: 42 }),
      ]),
    ]);

    expect(survey.stills).toBe(1);
    expect(survey.videos).toBe(1);
    expect(survey.still.maxLongEdge).toBe(2048);
    expect(survey.video.maxLongEdge).toBe(1280);
    expect(survey.longestVideoSeconds).toBe(42);
  });

  it('counts the clips longer than fifteen minutes', () => {
    const survey = summarize([
      album([
        asset({ kind: 'video', durationSeconds: VIDEO_SECONDS_CAP + 1 }),
        asset({ kind: 'video', durationSeconds: VIDEO_SECONDS_CAP }),
      ]),
    ]);

    expect(survey.longVideos).toBe(1);
  });

  it('tallies the resource inventory, which is where a full-size original would show', () => {
    const survey = summarize([
      album([
        asset({ resourceTypes: ['photo'] }),
        asset({ resourceTypes: ['photo', 'fullSizePhoto'] }),
      ]),
    ]);

    expect(survey.resourceTypes).toEqual({ photo: 2, fullSizePhoto: 1 });
    expect(survey.sourceTypes).toEqual({ typeCloudShared: 2 });
  });

  it('spans the whole set rather than one album', () => {
    const older = asset({ createdAt: 1_600_000_000_000 });
    const newer = asset({ createdAt: 1_800_000_000_000 });
    const survey = summarize([album([older], 'A'), album([newer], 'B')]);

    expect(survey.oldest).toBe(1_600_000_000_000);
    expect(survey.newest).toBe(1_800_000_000_000);
  });

  it('reads an empty phone as empty', () => {
    const survey = summarize([]);

    expect(survey.assets).toBe(0);
    expect(survey.still.maxLongEdge).toBeNull();
    expect(survey.oldest).toBeNull();
  });

  it('carries the album id through, so a row can be ticked', () => {
    const survey = summarize([album([asset()], 'Iceland')]);

    expect(survey.albums[0].localId).toBe('ph://album-Iceland');
  });

  it('describes only the albums it is given, which is what unticking one does', () => {
    const both = [album([asset(), asset()], 'A'), album([asset()], 'B')];

    expect(summarize(both).assets).toBe(3);
    expect(summarize(both.slice(0, 1)).assets).toBe(2);
  });
});

describe('assetsOf', () => {
  it('returns each asset once, newest first', () => {
    const shared = asset({ createdAt: 1_700_000_000_000 });
    const newer = asset({ createdAt: 1_900_000_000_000 });
    const older = asset({ createdAt: 1_500_000_000_000 });

    const found = assetsOf([album([shared, older], 'A'), album([shared, newer], 'B')]);

    expect(found).toEqual([newer, shared, older]);
  });
});

describe('pickSample', () => {
  it('spreads across kinds before it takes a second of anything', () => {
    const still = asset();
    const live = asset({ isLive: true });
    const video = asset({ kind: 'video' });
    const spare = asset();

    const sample = pickSample([still, spare, live, video]);

    expect(sample).toEqual([still, live, video]);
  });

  it('fills with whatever is newest when a kind is missing', () => {
    const first = asset();
    const second = asset();
    const third = asset();

    expect(pickSample([first, second, third])).toEqual([first, second, third]);
  });

  it('never asks for more than there is', () => {
    const only = asset({ kind: 'video' });

    expect(pickSample([only])).toEqual([only]);
  });

  it('takes the size it is given, still spread across kinds first', () => {
    const stills = [asset(), asset(), asset(), asset()];
    const video = asset({ kind: 'video' });

    const sample = pickSample([...stills, video], 2);

    expect(sample).toEqual([stills[0], video]);
  });

  it('runs to the end of a short library rather than padding to the size', () => {
    expect(pickSample([asset(), asset()], 100)).toHaveLength(2);
  });
});

describe('throughputMbPerSecond', () => {
  it('reads bytes and milliseconds as megabytes a second', () => {
    expect(throughputMbPerSecond(10_000_000, 2_000)).toBeCloseTo(5);
  });

  it('refuses to quote a rate for something that took no time', () => {
    expect(throughputMbPerSecond(10_000_000, 0)).toBeNull();
  });
});

describe('formatDuration', () => {
  it('stays in seconds below a minute', () => {
    expect(formatDuration(42)).toBe('42s');
  });

  it('pads the seconds once there are minutes', () => {
    expect(formatDuration(64)).toBe('1:04');
    expect(formatDuration(900)).toBe('15:00');
  });

  it('reads a still as zero rather than as an error', () => {
    expect(formatDuration(0)).toBe('0s');
  });
});
