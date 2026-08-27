import type { Noun, Target } from '@photobackup/core';
import { useSyncExternalStore } from 'react';

/**
 * A request to put something into an album, from wherever it was made.
 *
 * The target is captured at the moment the row was tapped rather than read when
 * an album is picked: choosing one takes long enough that the selection may
 * have changed, and every position in it would then mean a different
 * photograph.
 */
export interface FileRequest {
  target: Target;
  /** What to call it in the notice. A tile's menu knows; a selection does not. */
  noun?: Noun;
  /**
   * The one photograph this is about, when it is about one. What makes the
   * picker draw ticks for the albums it is already in — see core's
   * `useMembership`, which is deliberately never asked about several.
   */
  assetId?: string;
}

/**
 * The album picker, opened from two places that cannot reach each other.
 *
 * The picker itself is mounted once, beside the floating controls in the root
 * layout, because it has to outlive the surface that asked for it: the peek is
 * a `Modal` and closes before the sheet opens, and a selection sheet dismisses
 * itself on the way past. Neither can host a sheet that is still on screen
 * after it has gone.
 *
 * A module-level store rather than a context, for the reason `state/browsing.ts`
 * gives and `useVault`'s `askToUnlock` gives in the browser: there is one
 * archive and one thing being filed at a time, and threading a callback from
 * the root layout down through the grid, the peek and the modal inside it would
 * be ceremony around a single value.
 */
let held: FileRequest | null = null;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function snapshot(): FileRequest | null {
  return held;
}

function publish(next: FileRequest | null): void {
  held = next;
  for (const listener of listeners) listener();
}

/** Opens the picker over whatever is on screen. */
export function askToFile(request: FileRequest): void {
  publish(request);
}

export function stopFiling(): void {
  publish(null);
}

/** What is being filed, or null when the picker is shut. */
export function useFiling(): FileRequest | null {
  return useSyncExternalStore(subscribe, snapshot);
}
