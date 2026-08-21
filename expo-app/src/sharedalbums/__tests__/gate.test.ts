import { CALM_GAP_MS, MAX_GAP_MS } from '../run';
import { SharedFetchGate } from '../gate';
import type { Clock } from '../../sync/types';

/** A clock whose sleep jumps forward, so a paced run costs no real time. */
function testClock(): Clock & { waits: number[] } {
  let at = 0;
  const waits: number[] = [];
  return {
    waits,
    now: () => at,
    sleep: async (ms: number) => {
      waits.push(ms);
      at += ms;
    },
    random: () => 0.5,
  };
}

/** Lets every microtask the gate queued run, without any time passing. */
function flush(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

/** Resolves when told to, so two downloads can be in flight at once on purpose. */
function held<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

test('the first download is not made to wait for anything', async () => {
  const clock = testClock();
  const gate = new SharedFetchGate(clock);

  await gate.run(async () => 'done');

  expect(clock.waits).toEqual([]);
});

test('a second download waits out the gap since the first one ended', async () => {
  const clock = testClock();
  const gate = new SharedFetchGate(clock);

  await gate.run(async () => 1);
  await gate.run(async () => 2);

  expect(clock.waits).toEqual([CALM_GAP_MS]);
});

// The whole reason the gate exists: three upload workers each open whatever the
// queue handed them, and without this all three would be talking to iCloud.
test('two downloads asked for at once are run one after the other', async () => {
  const gate = new SharedFetchGate(testClock());
  const first = held<string>();
  const order: string[] = [];

  const one = gate.run(async () => {
    order.push('one started');
    await first.promise;
    order.push('one finished');
  });
  const two = gate.run(async () => {
    order.push('two started');
  });

  // Nothing has resolved the first, so the second must not have begun.
  await flush();
  expect(order).toEqual(['one started']);

  first.resolve('go');
  await Promise.all([one, two]);
  expect(order).toEqual(['one started', 'one finished', 'two started']);
});

test('a failure stretches the gap and a success shrinks it again', async () => {
  const gate = new SharedFetchGate(testClock());
  const calm = gate.gapMs();

  await expect(
    gate.run(async () => {
      throw new Error('iCloud said no');
    })
  ).rejects.toThrow('iCloud said no');
  const strained = gate.gapMs();

  await gate.run(async () => 'fine');

  expect(strained).toBeGreaterThan(calm);
  expect(gate.gapMs()).toBe(calm);
});

test('the gap stops growing once it is as long as it is allowed to be', async () => {
  const gate = new SharedFetchGate(testClock());

  for (let i = 0; i < 20; i++) {
    await gate.run(async () => {
      throw new Error('still no');
    }).catch(() => {});
  }

  expect(gate.gapMs()).toBe(MAX_GAP_MS);
});

// A gate that let one failure reject everything queued behind it would fail a
// whole batch of assets over one bad download.
test('a download that failed does not fail the ones queued behind it', async () => {
  const gate = new SharedFetchGate(testClock());

  const failing = gate.run(async () => {
    throw new Error('the first one broke');
  });
  const following = gate.run(async () => 'the second one is fine');

  await expect(failing).rejects.toThrow('the first one broke');
  await expect(following).resolves.toBe('the second one is fine');
});

// The pause is between requests. Measuring it from the start of the last one
// would let a slow fetch satisfy it by taking a long time, which is not a pause.
test('the wait is measured from when the last download ended', async () => {
  const clock = testClock();
  const gate = new SharedFetchGate(clock);

  await gate.run(async () => {
    // Time passing during a download, the way a real one does.
    await clock.sleep(5_000);
  });
  clock.waits.length = 0;

  await gate.run(async () => 'next');

  expect(clock.waits).toEqual([CALM_GAP_MS]);
});
