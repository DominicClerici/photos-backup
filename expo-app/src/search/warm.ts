import { useEffect } from 'react';
import { AppState, type AppStateStatus } from 'react-native';

import { warmSearch } from '@photobackup/core';

/**
 * Tells the archive that the gallery is open, so the models a search needs
 * start loading before anybody types.
 *
 * The phone's half of `web/src/components/SearchWarm.tsx`, and the same
 * reasoning: photo-ml holds no state and has never seen a screen, so the
 * encoder and the query parser used to be loaded when it started and never
 * given back — about 3GB of a 16GB card held all day for a search box nobody
 * had open. photod is what can tell the difference, and this is how it is told.
 *
 * Two moments rather than one, and on a phone the second is the important one.
 * The app is opened once and then lives in the background for days: it comes
 * back to the foreground far more often than it is launched, and every one of
 * those is a lease that has long since lapsed. `AppState` is the only thing
 * that sees it — a screen that was mounted before the phone was locked is still
 * mounted after, so no effect below this would run.
 *
 * Ordinary gallery traffic keeps the lease alive after that, so there is
 * nothing to poll: photod renews it on the requests it is already serving.
 *
 * Failures are swallowed. There is nothing to tell anybody — a search without
 * the models still returns photographs, ranked by captions, tags, recognised
 * text, filenames and place names, and the search screen says so when it
 * happens.
 */
export function useSearchWarmth(enabled: boolean) {
  useEffect(() => {
    if (!enabled) return;

    const warm = () => {
      void warmSearch().catch(() => {});
    };
    warm();

    const onChange = (next: AppStateStatus) => {
      if (next === 'active') warm();
    };
    const sub = AppState.addEventListener('change', onChange);
    return () => sub.remove();
  }, [enabled]);
}
