import type { SharedAsset, SharedFetchFailure, SharedFetchResult } from '../../../modules/photo-facts';
import type { Clock } from '../../sync/types';
import {
  describeFailure,
  isPermanent,
  Pacer,
  runFetches,
  SHARED_FETCH_BACKOFF,
  type FetchRun,
} from '../run';

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
    pixelWidth: 4032,
    pixelHeight: 3024,
    durationSeconds: 0,
    sourceTypes: { value: 2, names: ['typeCloudShared'] },
    resourceTypes: ['photo'],
    ...overrides,
  };
}

function ok(bytes = 1_000_000): SharedFetchResult {
  return {
    ok: true,
    read: {
      bytes,
      elapsedMs: 1_200,
      uniformTypeIdentifier: 'public.heic',
      originalFilename: 'IMG_0001.HEIC',
      resourceType: 'photo',
    },
  };
}

function failure(domain = 'CloudPhotoLibraryErrorDomain', code = 1001): SharedFetchFailure {
  return { domain, code, message: 'the operation could not be completed', bytes: 0, elapsedMs: 40 };
}

function failed(domain?: string, code?: number): SharedFetchResult {
  return { ok: false, failure: failure(domain, code) };
}

/** Never sleeps, and remembers every wait it was asked for. */
function testClock(): Clock & { waits: number[] } {
  const waits: number[] = [];
  return {
    waits,
    now: () => 0,
    sleep: async (ms: number) => {
      waits.push(ms);
    },
    // Fixed, so the jitter is a known multiplier and the delays are arithmetic
    // rather than a range. backoffDelay spans 0.5x to 1.5x; 0.5 is the middle.
    random: () => 0.5,
  };
}

function run(
  assets: SharedAsset[],
  read: (localId: string) => Promise<SharedFetchResult | null>,
  extra: { cancelled?: () => boolean; clock?: Clock & { waits: number[] } } = {}
) {
  const clock = extra.clock ?? testClock();
  const reports: FetchRun[] = [];
  const finished = runFetches(assets, {
    read,
    clock,
    onProgress: (state) => reports.push(state),
    cancelled: extra.cancelled ?? (() => false),
  });
  return { finished, reports, clock };
}

describe('isPermanent', () => {
  it('knows the module’s own "there is nothing to fetch"', () => {
    expect(isPermanent(failure('PhotoFacts', 404))).toBe(true);
  });

  it('treats an unrecognised cloud error as worth retrying', () => {
    expect(isPermanent(failure())).toBe(false);
  });
});

describe('describeFailure', () => {
  it('leads with the pair that will still mean something next release', () => {
    expect(
      describeFailure({ domain: 'PHPhotosErrorDomain', code: 3164, message: 'no network', bytes: 0, elapsedMs: 0 })
    ).toBe('PHPhotosErrorDomain 3164: no network');
  });

  it('says how far it got when it got anywhere', () => {
    expect(
      describeFailure({ domain: 'NSURLErrorDomain', code: -1001, message: 'timed out', bytes: 4096, elapsedMs: 0 })
    ).toContain('after 4096 bytes');
  });
});

describe('Pacer', () => {
  it('retries a cloud error and gives up only once the attempts are spent', () => {
    const pacer = new Pacer(() => 0.5);
    pacer.beginAsset();

    const verdicts = Array.from({ length: SHARED_FETCH_BACKOFF.maxAttempts }, () =>
      pacer.failed(failure())
    );

    expect(verdicts.slice(0, -1).map((v) => v.action)).toEqual(['retry', 'retry', 'retry']);
    expect(verdicts[verdicts.length - 1].action).toBe('give-up');
  });

  it('does not spend an asset’s attempts on a failure a retry cannot help', () => {
    const pacer = new Pacer(() => 0.5);
    pacer.beginAsset();

    const verdict = pacer.failed(failure('PhotoFacts', 404));

    expect(verdict.action).toBe('give-up');
  });

  it('slows down as failures accumulate and speeds back up as they stop', () => {
    const pacer = new Pacer(() => 0.5);
    const calm = pacer.gapMs();

    pacer.beginAsset();
    pacer.failed(failure());
    const strained = pacer.gapMs();

    pacer.succeeded();
    const recovering = pacer.gapMs();

    expect(strained).toBeGreaterThan(calm);
    expect(recovering).toBe(calm);
  });

  it('stops the run once several assets in a row have run out of attempts', () => {
    const pacer = new Pacer(() => 0.5, SHARED_FETCH_BACKOFF, 2);
    const exhaust = () => {
      pacer.beginAsset();
      let verdict = pacer.failed(failure());
      while (verdict.action === 'retry') {
        verdict = pacer.failed(failure());
      }
      return verdict;
    };

    expect(exhaust().action).toBe('give-up');
    expect(exhaust().action).toBe('stop');
  });

  it('forgets the run of failures as soon as one asset works', () => {
    const pacer = new Pacer(() => 0.5, SHARED_FETCH_BACKOFF, 2);
    const exhaust = () => {
      pacer.beginAsset();
      let verdict = pacer.failed(failure());
      while (verdict.action === 'retry') {
        verdict = pacer.failed(failure());
      }
      return verdict;
    };

    exhaust();
    pacer.succeeded();

    expect(exhaust().action).toBe('give-up');
  });

  it('backs off further with each attempt on the same asset', () => {
    const pacer = new Pacer(() => 0.5);
    pacer.beginAsset();

    const delays: number[] = [];
    let verdict = pacer.failed(failure());
    while (verdict.action === 'retry') {
      delays.push(verdict.delayMs);
      verdict = pacer.failed(failure());
    }

    expect(delays).toEqual([...delays].sort((a, b) => a - b));
    expect(delays[0]).toBe(SHARED_FETCH_BACKOFF.baseMs);
  });
});

describe('runFetches', () => {
  it('fetches every asset and totals only the bytes that arrived', async () => {
    const assets = [asset(), asset(), asset()];
    const { finished } = run(assets, async () => ok(500));

    const result = await finished;

    expect(result.status).toBe('done');
    expect(result.ok).toBe(3);
    expect(result.failed).toBe(0);
    expect(result.bytes).toBe(1500);
    expect(result.results.map((r) => r.asset)).toEqual(assets);
  });

  it('retries a failing asset and reports how many goes it took', async () => {
    let calls = 0;
    const { finished } = run([asset()], async () => {
      calls += 1;
      return calls < 3 ? failed() : ok();
    });

    const result = await finished;

    expect(result.ok).toBe(1);
    expect(result.results[0].attempts).toBe(3);
  });

  it('keeps going past an asset it had to give up on', async () => {
    const doomed = asset();
    const fine = asset();
    const { finished } = run([doomed, fine], async (localId) =>
      localId === doomed.localId ? failed('PhotoFacts', 404) : ok()
    );

    const result = await finished;

    expect(result.status).toBe('done');
    expect(result.failed).toBe(1);
    expect(result.ok).toBe(1);
    expect(result.results[0].error).toContain('PhotoFacts 404');
  });

  it('stops the run rather than hammering iCloud that keeps refusing', async () => {
    const assets = Array.from({ length: 20 }, () => asset());
    const { finished } = run(assets, async () => failed());

    const result = await finished;

    expect(result.status).toBe('stopped');
    expect(result.stoppedBecause).toContain('in a row');
    // Three assets, each spending its four attempts, and then nothing.
    expect(result.done).toBeLessThan(assets.length);
  });

  it('waits between attempts rather than retrying straight away', async () => {
    const clock = testClock();
    let calls = 0;
    const { finished } = run(
      [asset()],
      async () => {
        calls += 1;
        return calls < 2 ? failed() : ok();
      },
      { clock }
    );

    await finished;

    expect(clock.waits).toEqual([SHARED_FETCH_BACKOFF.baseMs]);
  });

  it('paces itself between assets even when nothing has gone wrong', async () => {
    const clock = testClock();
    const { finished } = run([asset(), asset(), asset()], async () => ok(), { clock });

    await finished;

    // One gap between each pair, and none before the first.
    expect(clock.waits).toHaveLength(2);
  });

  it('ends the run when the build cannot fetch at all', async () => {
    const { finished } = run([asset(), asset()], async () => null);

    const result = await finished;

    expect(result.status).toBe('stopped');
    expect(result.done).toBe(1);
    expect(result.stoppedBecause).toContain('cannot fetch');
  });

  it('stops at the next asset when it is told to', async () => {
    let stop = false;
    const { finished } = run(
      [asset(), asset(), asset()],
      async () => {
        stop = true;
        return ok();
      },
      { cancelled: () => stop }
    );

    const result = await finished;

    expect(result.status).toBe('cancelled');
    expect(result.stoppedBecause).toBe('stopped by hand');
    expect(result.done).toBe(1);
  });

  it('reports itself as it goes, and reports something new each time', async () => {
    const { finished, reports } = run([asset(), asset()], async () => ok());

    await finished;

    expect(reports.length).toBeGreaterThan(2);
    // A caller holding these in React state compares by identity, so a report
    // that is the same object as the last one never redraws.
    expect(new Set(reports).size).toBe(reports.length);
    expect(reports[reports.length - 1].current).toBeNull();
  });

  it('names the asset in flight, and lets go of it at the end', async () => {
    const only = asset();
    const { finished, reports } = run([only], async () => ok());

    await finished;

    expect(reports[0].current?.asset).toBe(only);
    expect(reports[reports.length - 1].current).toBeNull();
  });

  it('folds a native call that throws into a failure like any other', async () => {
    const { finished } = run([asset()], async () => {
      throw new Error('the bridge went away');
    });

    const result = await finished;

    expect(result.failed).toBe(1);
    expect(result.results[0].error).toContain('the bridge went away');
  });

  it('passes byte-level progress through to the asset it belongs to', async () => {
    const only = asset();
    const reports: FetchRun[] = [];
    let emit: ((progress: { localId: string; bytes: number; fraction: number }) => void) | null =
      null;

    await runFetches([only], {
      read: async () => {
        emit?.({ localId: only.localId, bytes: 2048, fraction: 0.5 });
        return ok();
      },
      clock: testClock(),
      onProgress: (state) => reports.push(state),
      cancelled: () => false,
      subscribe: (listener) => {
        emit = listener;
        return () => {
          emit = null;
        };
      },
    });

    expect(reports.some((r) => r.current?.bytes === 2048 && r.current.fraction === 0.5)).toBe(true);
  });

  it('ignores progress belonging to a fetch that has already finished', async () => {
    const first = asset();
    const second = asset();
    const reports: FetchRun[] = [];
    let emit: ((progress: { localId: string; bytes: number; fraction: number }) => void) | null =
      null;

    await runFetches([first, second], {
      read: async (localId) => {
        // A late event from the first asset, arriving while the second is in
        // flight. It must not be shown against the second.
        if (localId === second.localId) {
          emit?.({ localId: first.localId, bytes: 999_999, fraction: 1 });
        }
        return ok();
      },
      clock: testClock(),
      onProgress: (state) => reports.push(state),
      cancelled: () => false,
      subscribe: (listener) => {
        emit = listener;
        return () => {};
      },
    });

    expect(reports.every((r) => (r.current?.bytes ?? 0) < 999_999)).toBe(true);
  });
});
