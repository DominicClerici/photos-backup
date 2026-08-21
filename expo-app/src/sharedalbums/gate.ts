/**
 * The one door every shared-album download goes through, and the thing that
 * decides how fast it opens.
 *
 * The survey had a run loop of its own for this — see run.ts — because it *was*
 * the loop: it fetched a list of assets, one at a time, and paced itself between
 * them. The backup has no such loop. Its downloads happen inside the sync
 * engine's upload workers, three of them, each opening whatever the queue handed
 * it, and none of them aware that the other two are also talking to iCloud.
 *
 * So the pacing moves here, to something the workers share rather than something
 * a loop owns. It does two things:
 *
 *   One at a time.  Three concurrent workers is right for a LAN and wrong for
 *                   Apple: the point of the concurrency is that a slow upload
 *                   does not hold up the photos behind it, and that still holds
 *                   with the downloads serialized — one worker fetches while
 *                   another uploads what it fetched a moment ago.
 *   A gap that      Failures stretch the pause between downloads and successes
 *   breathes.       shrink it, on exactly the arithmetic the survey used, so a
 *                   phone that has annoyed iCloud backs off without anyone
 *                   having decided which error meant so.
 *
 * The retrying itself is deliberately not here. The engine already retries every
 * item with exponential backoff and gives up after five attempts, and a second
 * retry loop nested inside the first would multiply the two — an asset would
 * make twenty attempts and take an hour to be declared failed.
 *
 * Every failure raises the strain, including the ones that are nothing to do
 * with iCloud, such as a full disk. That is a deliberate simplification: it
 * costs an extra pause on a phone that is failing everything anyway, and the
 * alternative is a gate that has to be told whose fault each failure was — which
 * means the classification the survey established cannot be made honestly.
 */

import { strainCeiling, strainedGapMs } from './run';
import { systemClock, type Clock } from '../sync/types';

export class SharedFetchGate {
  private strain = 0;
  /**
   * When the last download finished, and null before there has been one.
   *
   * Null rather than zero, because zero is a real instant: with the system clock
   * it is 1970 and every gap has long since elapsed, which is the right answer by
   * accident, and with any clock that starts at zero the first download would be
   * made to sit through a pause it has nothing to be polite about.
   */
  private endedAt: number | null = null;
  /**
   * The tail of the queue: whatever is currently waiting or downloading. New
   * callers chain onto it, which is what serializes them.
   */
  private turn: Promise<unknown> = Promise.resolve();

  constructor(private readonly clock: Clock = systemClock) {}

  /** The pause a download would be asked to wait through right now. */
  gapMs(): number {
    return strainedGapMs(this.strain);
  }

  /**
   * Runs one download, once it is this caller's turn and the pause since the
   * last one has elapsed.
   *
   * Whatever `work` throws is rethrown unchanged. The gate has an opinion about
   * pace and none about failure: the engine is the one that decides what a
   * failed download costs an item.
   */
  async run<T>(work: () => Promise<T>): Promise<T> {
    const mine = this.turn.then(() => this.take(work));
    // Swallowed only for the *queue*, so that one failed download does not
    // reject every caller queued behind it. The caller of that download still
    // gets the rejection, through `mine`.
    this.turn = mine.then(
      () => {},
      () => {}
    );
    return mine;
  }

  private async take<T>(work: () => Promise<T>): Promise<T> {
    // Measured from the end of the last download rather than from its start, so
    // the gap is a pause between requests and not a rate limit of our own that
    // a slow fetch could silently satisfy.
    if (this.endedAt !== null) {
      const waited = this.clock.now() - this.endedAt;
      const wait = Math.max(0, this.gapMs() - waited);
      if (wait > 0) await this.clock.sleep(wait);
    }

    try {
      const value = await work();
      // One step back, not a reset: a single success in a bad patch is not
      // evidence the patch is over. Same rule as Pacer.succeeded().
      this.strain = Math.max(0, this.strain - 1);
      return value;
    } catch (e) {
      this.strain = Math.min(strainCeiling(), this.strain + 1);
      throw e;
    } finally {
      this.endedAt = this.clock.now();
    }
  }
}
