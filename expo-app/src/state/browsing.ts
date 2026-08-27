import type { TimelineState } from '@photobackup/core/react';
import { useEffect, useSyncExternalStore } from 'react';

/**
 * The timeline somebody is looking into, for the screen that is looking *at* one
 * photograph out of it.
 *
 * The viewer is a route rather than a view inside the grid — that is the whole
 * point of it, and what makes the Back gesture close it the way
 * `history.pushState` does in the browser (WEB_TO_MOBILE § 4). But a route is a
 * sibling of the tab it was opened from, not a child, so it cannot read a
 * context the gallery provides, and a viewer that mounted its own `useTimeline`
 * would refetch the day table and the page it is already showing — a spinner
 * over a photograph that is on screen.
 *
 * So the timeline is published here and read from there. It is the shape
 * `src/archive.ts` already uses and gives the same reason for: there is one
 * archive, and there is exactly one collection being browsed at a time, so a
 * module-level value is honest where threading a provider through the router
 * would be ceremony. What is different is that a `TimelineState` changes, so it
 * is a store with subscribers rather than a variable — `useTimeline` memoizes
 * its return and bumps the identity when a page lands, which is precisely the
 * signal the viewer needs to notice that the photograph it is waiting for has
 * arrived.
 *
 * Phase 5's collections publish their own here before pushing the same route.
 */
let held: TimelineState | null = null;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function snapshot(): TimelineState | null {
  return held;
}

function publish(next: TimelineState | null): void {
  // `useTimeline` returns a memoized object whose identity changes exactly when
  // something a reader could see has changed, so this is both the cheap check
  // and the correct one.
  if (held === next) return;
  held = next;
  for (const listener of listeners) listener();
}

/**
 * Announces this screen's timeline as the one being browsed.
 *
 * Published on every render rather than on a dependency, because the identity
 * is the dependency — see above. Retracted only on unmount, so that the
 * re-publish between two renders is never a moment where a viewer already open
 * is told there is nothing to look at.
 */
export function useBrowsing(timeline: TimelineState): void {
  useEffect(() => {
    publish(timeline);
  });

  useEffect(
    () => () => {
      publish(null);
    },
    [],
  );
}

/**
 * The timeline being browsed, or null when nothing is — a deep link straight to
 * the viewer with no gallery behind it, which is a route to send back to the
 * grid rather than a state to draw.
 */
export function useBrowsed(): TimelineState | null {
  return useSyncExternalStore(subscribe, snapshot);
}
