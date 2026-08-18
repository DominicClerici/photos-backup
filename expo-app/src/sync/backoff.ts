import type { Clock } from './types';

export type BackoffPolicy = {
  baseMs: number;
  maxMs: number;
  /** Attempts allowed before an item is given up on. Infinity never gives up. */
  maxAttempts: number;
};

/** Item failures: five tries over roughly ten minutes, then park it as failed. */
export const ITEM_BACKOFF: BackoffPolicy = {
  baseMs: 1_000,
  maxMs: 5 * 60_000,
  maxAttempts: 5,
};

/**
 * Transport failures never give up. "The server is off" is a normal condition
 * for a home archive, and the queue is supposed to sit and wait through it.
 */
export const TRANSPORT_BACKOFF: BackoffPolicy = {
  baseMs: 1_000,
  maxMs: 60_000,
  maxAttempts: Infinity,
};

/**
 * Exponential with jitter, spanning 0.5x to 1.5x of the nominal delay. The
 * jitter matters less here than in a fleet — there is one phone — but it keeps a
 * whole batch of items that failed together from retrying in lockstep.
 */
export function backoffDelay(attempts: number, policy: BackoffPolicy, random: number): number {
  const exponent = Math.max(0, attempts - 1);
  const nominal = Math.min(policy.maxMs, policy.baseMs * 2 ** exponent);
  return Math.round(nominal * (0.5 + random));
}

/**
 * Pauses all work while the server is unreachable.
 *
 * This exists to keep a server outage from being charged to the items. Without
 * it, one restart of photod during a backfill burns every item's retries and
 * leaves the library sitting in `failed`.
 */
export class CircuitBreaker {
  private failures = 0;
  private openUntil = 0;

  constructor(
    private readonly clock: Clock,
    private readonly policy: BackoffPolicy = TRANSPORT_BACKOFF
  ) {}

  get retryAt(): number {
    return this.openUntil;
  }

  isOpen(): boolean {
    return this.openUntil > this.clock.now();
  }

  trip(): number {
    this.failures += 1;
    const delay = backoffDelay(this.failures, this.policy, this.clock.random());
    this.openUntil = this.clock.now() + delay;
    return delay;
  }

  reset(): void {
    this.failures = 0;
    this.openUntil = 0;
  }

  /** Waits out an open breaker. Returns the milliseconds spent waiting. */
  async wait(): Promise<number> {
    const remaining = this.openUntil - this.clock.now();
    if (remaining <= 0) return 0;
    await this.clock.sleep(remaining);
    return remaining;
  }
}
