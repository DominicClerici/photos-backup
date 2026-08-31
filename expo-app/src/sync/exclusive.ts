/**
 * One backup run at a time, whoever started it.
 *
 * The engine's freedom from locking rests on a single sentence in
 * `SyncEngine`'s comment: it is the only writer to the store, and JavaScript is
 * single-threaded, so no item needs an in-flight state. That held while the
 * Backup tab was the only thing that could start a run. It stops holding the
 * moment iOS can wake the app and start a second one — the two engines would be
 * in the same runtime but not in the same loop, and each would hand the other's
 * rows to a transport that had already claimed them.
 *
 * So the invariant is made explicit rather than left to arithmetic about who
 * can press what. A run holds this for its whole life; anything that cannot
 * take it does not start, and says why.
 */

export type RunHolder = 'foreground' | 'background';

let holder: RunHolder | null = null;

/**
 * Takes the run lock, or returns null when somebody else has it.
 *
 * The release is handed back rather than exposed as a second function so a
 * caller cannot release a lock it never took — the background task and the
 * Backup tab are far enough apart that the mistake would be invisible.
 */
export function acquireRun(who: RunHolder): (() => void) | null {
  if (holder !== null) return null;
  holder = who;

  let released = false;
  return () => {
    if (released) return;
    released = true;
    holder = null;
  };
}

/** Who holds the run lock, or null when nothing is running. */
export function runHolder(): RunHolder | null {
  return holder;
}
