import {
  backoffDelay,
  CircuitBreaker,
  ITEM_BACKOFF,
  TRANSPORT_BACKOFF,
} from '../backoff';
import { TestClock } from './fakes';

test('delay grows with each attempt', () => {
  const delays = [1, 2, 3, 4].map((attempt) => backoffDelay(attempt, ITEM_BACKOFF, 0.5));
  expect(delays).toEqual([1_000, 2_000, 4_000, 8_000]);
});

test('delay is capped', () => {
  const delay = backoffDelay(30, ITEM_BACKOFF, 0.5);
  expect(delay).toBeLessThanOrEqual(ITEM_BACKOFF.maxMs);
});

test('jitter stays within half and one and a half of the nominal delay', () => {
  for (const random of [0, 0.25, 0.5, 0.75, 0.999]) {
    const delay = backoffDelay(3, ITEM_BACKOFF, random);
    expect(delay).toBeGreaterThanOrEqual(4_000 * 0.5);
    expect(delay).toBeLessThanOrEqual(4_000 * 1.5);
  }
});

test('the first attempt is not penalized by the exponent', () => {
  expect(backoffDelay(0, ITEM_BACKOFF, 0.5)).toBe(ITEM_BACKOFF.baseMs);
  expect(backoffDelay(1, ITEM_BACKOFF, 0.5)).toBe(ITEM_BACKOFF.baseMs);
});

test('the breaker opens, holds, then closes once waited out', async () => {
  const clock = new TestClock();
  const breaker = new CircuitBreaker(clock, TRANSPORT_BACKOFF);

  expect(breaker.isOpen()).toBe(false);

  const delay = breaker.trip();
  expect(delay).toBeGreaterThan(0);
  expect(breaker.isOpen()).toBe(true);

  await breaker.wait();
  expect(breaker.isOpen()).toBe(false);
});

test('consecutive trips back off further', () => {
  const clock = new TestClock();
  const breaker = new CircuitBreaker(clock, TRANSPORT_BACKOFF);

  const first = breaker.trip();
  const second = breaker.trip();

  expect(second).toBeGreaterThan(first);
});

test('a success resets the backoff', () => {
  const clock = new TestClock();
  const breaker = new CircuitBreaker(clock, TRANSPORT_BACKOFF);

  breaker.trip();
  breaker.trip();
  breaker.reset();

  expect(breaker.isOpen()).toBe(false);
  expect(breaker.trip()).toBe(backoffDelay(1, TRANSPORT_BACKOFF, 0.5));
});

test('transport backoff never gives up', () => {
  expect(TRANSPORT_BACKOFF.maxAttempts).toBe(Infinity);
});
