/**
 * Driving a run of shared-asset fetches: how fast to ask, when to try again, and
 * when to conclude that iCloud has had enough of being asked.
 *
 * Pure, and importing nothing that needs a phone, for the reason summary.ts is:
 * this is the part most likely to be wrong and least likely to be exercised by
 * hand. Provoking a rate limit on purpose is slow, unreliable and something
 * Apple can change on a Tuesday; a fake `read` that fails on demand costs
 * nothing and tests the same loop.
 *
 * The shape of the problem. A shared asset is not on the phone, so every one of
 * these is a network round trip to Apple, and a run of five hundred is five
 * hundred of them back to back — the exact behaviour a service throttles. What
 * throttling looks like from here is unknown: PhotoKit surfaces whatever NSError
 * the cloud layer produced, and which domain and code mean "slow down" is not
 * documented anywhere that can be relied on.
 *
 * So this is built to be wrong about the diagnosis and right about the
 * response. It retries almost everything, because the cost of retrying a
 * permanent failure is a few seconds and the cost of abandoning a transient one
 * is a hole in a backup. It slows down as failures accumulate and speeds back up
 * as they stop, so a phone that has annoyed iCloud backs away without being told
 * which error meant so. And it stops the whole run outright once several assets
 * in a row have exhausted their attempts, because at that point the difference
 * between a rate limit and a dead network no longer matters: continuing is
 * hammering, and the run has already collected the evidence.
 */

import type {
  SharedAsset,
  SharedFetchFailure,
  SharedFetchProgress,
  SharedFetchResult,
  SharedResourceRead,
} from '../../modules/photo-facts';
import { backoffDelay, type BackoffPolicy } from '../sync/backoff';
import { errorText, type Clock } from '../sync/types';

/**
 * Four tries per asset over roughly half a minute.
 *
 * Slower to start than the queue's item backoff, which begins at a second: that
 * one is retrying a server on the same network, and this one is retrying a
 * service that may have just asked to be left alone. A first retry two seconds
 * after a refusal is not really a change of behaviour.
 */
export const SHARED_FETCH_BACKOFF: BackoffPolicy = {
  baseMs: 2_000,
  maxMs: 60_000,
  maxAttempts: 4,
};

/** The pause between two fetches when nothing has gone wrong. */
export const CALM_GAP_MS = 150;

/** The longest that pause is allowed to grow to as failures accumulate. */
export const MAX_GAP_MS = 10_000;

/**
 * How many assets may exhaust their attempts back to back before the run gives
 * up on the rest.
 *
 * A single asset failing four times is ordinary — it may have been withdrawn, or
 * be the one video on a flaky connection. Three in a row is a condition rather
 * than a coincidence, and nothing that follows will succeed either.
 */
export const GIVE_UP_AFTER = 3;

/**
 * Failures a retry cannot help with.
 *
 * Deliberately almost empty. Every code not named here is retried, including
 * ones that turn out to be permanent, because this list is a claim about Apple's
 * error codes and the whole reason for the probe is that no such claim can yet
 * be made honestly. Being wrong in this direction costs four attempts and half a
 * minute; being wrong in the other costs an asset that would have come back.
 */
const PERMANENT: readonly { domain: string; code: number }[] = [
  // The module's own: the asset carries no fetchable resource at all.
  { domain: 'PhotoFacts', code: 404 },
  // NSUserCancelledError. Asking again is asking to be cancelled again.
  { domain: 'NSCocoaErrorDomain', code: 3072 },
];

export function isPermanent(failure: SharedFetchFailure): boolean {
  return PERMANENT.some((one) => one.domain === failure.domain && one.code === failure.code);
}

/**
 * A failure in one line, leading with the pair that identifies it.
 *
 * Domain and code first because they are the durable part: the message is
 * localized and Apple rewrites it between releases, while the pair is what will
 * be recognised the second time it happens.
 */
export function describeFailure(failure: SharedFetchFailure): string {
  const partial = failure.bytes > 0 ? ` after ${failure.bytes} bytes` : '';
  return `${failure.domain} ${failure.code}${partial}: ${failure.message}`;
}

export type Verdict =
  | { action: 'retry'; delayMs: number; attempt: number }
  | { action: 'give-up'; reason: string }
  | { action: 'stop'; reason: string };

/**
 * How hard the run is currently leaning on iCloud, and what to do about the
 * failure that just came back.
 *
 * Two counters that look similar and are not. `attempt` is per asset and resets
 * for each one; it decides whether this asset gets another go. `consecutive`
 * counts assets that ran out of attempts with no success in between, and it
 * decides whether the run continues at all.
 *
 * `strain` is the third and the only one that is not a countdown: it rises with
 * every failure and falls with every success, and all it does is stretch the
 * pause between assets. That is what makes the backing off gradual rather than
 * a cliff — a run that hits trouble halfway slows to a crawl, and recovers to
 * full speed on its own if the trouble passes.
 */
export class Pacer {
  private attempt = 0;
  private consecutive = 0;
  private strain = 0;

  constructor(
    private readonly random: () => number,
    private readonly policy: BackoffPolicy = SHARED_FETCH_BACKOFF,
    private readonly giveUpAfter: number = GIVE_UP_AFTER
  ) {}

  /** How long to wait before starting the next asset. */
  gapMs(): number {
    return strainedGapMs(this.strain);
  }

  beginAsset(): void {
    this.attempt = 0;
  }

  succeeded(): void {
    this.attempt = 0;
    this.consecutive = 0;
    // One step back rather than a reset. A single success in the middle of a
    // bad patch is not evidence that the patch is over, and dropping straight
    // back to full speed on it is how a run oscillates instead of recovering.
    this.strain = Math.max(0, this.strain - 1);
  }

  failed(failure: SharedFetchFailure): Verdict {
    this.attempt += 1;

    // A permanent failure is this asset's problem and nobody else's. It does not
    // raise the strain and does not count towards stopping the run, because
    // neither of those is about an asset — they are about iCloud, which has said
    // nothing here beyond "not that one".
    if (isPermanent(failure)) return { action: 'give-up', reason: describeFailure(failure) };

    // Capped where the gap stops growing anyway, so the number stays a
    // description of the run rather than an ever-climbing counter.
    this.strain = Math.min(strainCeiling(), this.strain + 1);

    if (this.attempt < this.policy.maxAttempts) {
      return {
        action: 'retry',
        attempt: this.attempt,
        delayMs: backoffDelay(this.attempt, this.policy, this.random()),
      };
    }

    this.consecutive += 1;
    const reason = `${describeFailure(failure)} — gave up after ${this.attempt} attempts`;
    if (this.consecutive >= this.giveUpAfter) {
      return {
        action: 'stop',
        reason: `${this.consecutive} assets in a row ran out of attempts. Last was ${reason}`,
      };
    }
    return { action: 'give-up', reason };
  }
}

/**
 * The pause between two fetches at a given level of strain: the calm gap
 * doubled once per failure, up to the ceiling.
 *
 * Exported because the backup paces itself the same way and must not pace itself
 * *differently* — two ideas of how hard iCloud may be leaned on, drifting apart
 * over releases, is exactly the kind of thing nobody would notice going wrong.
 * See src/sharedalbums/gate.ts.
 */
export function strainedGapMs(strain: number): number {
  return Math.min(MAX_GAP_MS, Math.round(CALM_GAP_MS * 2 ** strain));
}

/** Where doubling the gap stops making any difference. */
export function strainCeiling(): number {
  return Math.ceil(Math.log2(MAX_GAP_MS / CALM_GAP_MS));
}

/** One sample asset, and what asking iCloud for it produced. */
export type SampleRead = {
  asset: SharedAsset;
  read: SharedResourceRead | null;
  /** Null when the fetch succeeded. Set to whatever went wrong when it did not. */
  error: string | null;
  /** How many goes it took, successful or not. */
  attempts: number;
};

export type RunStatus = 'running' | 'done' | 'stopped' | 'cancelled';

/**
 * Everything on screen while a run is going, and everything left of it after.
 *
 * `bytes` counts successful reads only. Bytes that arrived before a failure are
 * real and are worth knowing about, but they are reported against the failure
 * that lost them rather than added to a total that reads as "downloaded".
 */
export type FetchRun = {
  total: number;
  done: number;
  ok: number;
  failed: number;
  bytes: number;
  /** The asset being fetched now, and how far into it the run is. */
  current: {
    asset: SharedAsset;
    /** 1 on the first go. Anything higher is a retry, and worth showing as one. */
    attempt: number;
    bytes: number;
    fraction: number;
  } | null;
  /** The pause now being waited out, in milliseconds, or 0 when none is. */
  waitingMs: number;
  status: RunStatus;
  /** Why the run ended early, when it did. */
  stoppedBecause: string | null;
  results: SampleRead[];
};

export type RunDeps = {
  read: (localId: string) => Promise<SharedFetchResult | null>;
  clock: Clock;
  onProgress: (run: FetchRun) => void;
  /** Asked before every attempt. True ends the run at the next boundary. */
  cancelled: () => boolean;
  /** Where byte-level progress arrives from, on a build that reports any. */
  subscribe?: (listener: (progress: SharedFetchProgress) => void) => () => void;
};

/**
 * Fetches each asset in turn, and reports itself all the way through.
 *
 * One at a time, which was true of the three-asset sample for a reason that has
 * not changed and now has a second: the elapsed time per asset is a measurement
 * and parallel fetches would each report the others' contention, and a run of
 * hundreds in parallel is the most reliable way to find out what a rate limit
 * feels like without meaning to.
 *
 * Cancellation is checked between attempts and not during one. A fetch already
 * in flight is left to finish, because PhotoKit's cancel takes a request id this
 * would have to hold and thread back, and the wait it saves is one asset.
 */
export async function runFetches(assets: SharedAsset[], deps: RunDeps): Promise<FetchRun> {
  const run: FetchRun = {
    total: assets.length,
    done: 0,
    ok: 0,
    failed: 0,
    bytes: 0,
    current: null,
    waitingMs: 0,
    status: 'running',
    stoppedBecause: null,
    results: [],
  };

  // A copy per report. React compares by identity and the caller is holding this
  // in state, so handing out the same object every time would move the numbers
  // and never redraw them.
  const emit = () => deps.onProgress({ ...run, results: [...run.results] });

  const unsubscribe = deps.subscribe?.((progress) => {
    // Events from the fetch before this one can still be in flight, and dropping
    // them here is cheaper than any attempt to drain them at the source.
    if (!run.current || run.current.asset.localId !== progress.localId) return;
    run.current = { ...run.current, bytes: progress.bytes, fraction: progress.fraction };
    emit();
  });

  const pacer = new Pacer(deps.clock.random);

  try {
    for (const asset of assets) {
      if (deps.cancelled()) {
        run.status = 'cancelled';
        break;
      }

      // Not before the first, where there is nothing to be polite about yet.
      if (run.done > 0) {
        run.waitingMs = pacer.gapMs();
        emit();
        await deps.clock.sleep(run.waitingMs);
        run.waitingMs = 0;
      }

      pacer.beginAsset();
      const outcome = await fetchOne(asset, run, pacer, deps, emit);

      run.results.push(outcome.result);
      run.done += 1;
      if (outcome.result.error === null) run.ok += 1;
      else run.failed += 1;
      run.current = null;
      emit();

      if (outcome.stop) {
        run.status = 'stopped';
        run.stoppedBecause = outcome.stop;
        break;
      }
      if (outcome.cancelled) {
        run.status = 'cancelled';
        break;
      }
    }

    if (run.status === 'running') run.status = 'done';
    if (run.status === 'cancelled') run.stoppedBecause = 'stopped by hand';
  } finally {
    unsubscribe?.();
  }

  run.current = null;
  run.waitingMs = 0;
  emit();
  return run;
}

type Outcome = { result: SampleRead; stop?: string; cancelled?: boolean };

async function fetchOne(
  asset: SharedAsset,
  run: FetchRun,
  pacer: Pacer,
  deps: RunDeps,
  emit: () => void
): Promise<Outcome> {
  let attempt = 0;

  for (;;) {
    attempt += 1;
    run.current = { asset, attempt, bytes: 0, fraction: 0 };
    emit();

    let result: SharedFetchResult | null;
    try {
      result = await deps.read(asset.localId);
    } catch (e) {
      // The native call is not supposed to reject any more — it reports failures
      // as data. A build that predates that still throws, and one that throws
      // for some third reason should not take the run down with it, so an
      // exception is folded into the shape everything else here already has.
      result = {
        ok: false,
        failure: { domain: 'thrown', code: 0, message: errorText(e), bytes: 0, elapsedMs: 0 },
      };
    }

    // Null is the one thing it means everywhere: this build cannot. Nothing
    // further in the list will fare any better, so the run ends rather than
    // recording the same answer several hundred times.
    if (result === null) {
      const reason = 'this dev client cannot fetch shared assets';
      return { result: { asset, read: null, error: reason, attempts: attempt }, stop: reason };
    }

    if (result.ok) {
      pacer.succeeded();
      run.bytes += result.read.bytes;
      return { result: { asset, read: result.read, error: null, attempts: attempt } };
    }

    const verdict = pacer.failed(result.failure);
    if (verdict.action === 'retry') {
      if (deps.cancelled()) {
        return {
          result: {
            asset,
            read: null,
            error: describeFailure(result.failure),
            attempts: attempt,
          },
          cancelled: true,
        };
      }

      run.waitingMs = verdict.delayMs;
      emit();
      await deps.clock.sleep(verdict.delayMs);
      run.waitingMs = 0;
      continue;
    }

    return {
      result: { asset, read: null, error: verdict.reason, attempts: attempt },
      stop: verdict.action === 'stop' ? verdict.reason : undefined,
    };
  }
}
