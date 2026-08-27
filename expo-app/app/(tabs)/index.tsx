import { useTrashActions, useView } from '@photobackup/core/react';

import { useCachedTimeline } from '../../src/gallery/cache';
import { Grid } from '../../src/grid';
import { useBrowsing } from '../../src/state/browsing';

/**
 * The timeline.
 *
 * `useTimeline` is core's, unmodified: the sparse-array-over-a-day-table store
 * with its own fetch scheduler, which is the most load-bearing thing in the
 * gallery and touches nothing but `fetch`, `AbortController` and refs. The
 * browser mounts the same hook against the same endpoints; what differs is the
 * transport underneath it, installed once in `src/archive.ts`, and the fact
 * that every rendition here carries the device token in a header rather than a
 * cookie. See WEB_TO_MOBILE § 3.2.
 *
 * No filter: this is the library. The view is the floating sort-and-filter
 * control's, held above the router so that it survives the grid re-rendering
 * under it and is dropped when the grid goes away — a view is of one timeline,
 * and "the videos in this album, longest first" is not a thing to still be
 * looking at somewhere else.
 *
 * `useCachedTimeline` is `useTimeline` with the third argument filled: the
 * offline store WEB_TO_MOBILE § 3.6 asked to be built in Phase 3 and filled in
 * Phase 6. What it buys is that this screen has its geometry and the ground
 * already walked before the archive has said a word, and something to draw when
 * it never does. The reload it hands back is the same `retry` plus the drop
 * that keeps a write from leaving last week's timeline behind it.
 *
 * `useTrashActions` is core's too, and it is what the floating selection
 * control spends: this screen is the only thing that knows which timeline the
 * positions in a selection are counted in, so it is the thing that says what
 * can be done to them.
 *
 * `useBrowsing` announces it as the timeline being browsed, which is how the
 * viewer — a route, and so a sibling of this screen rather than a child — reads
 * the same store instead of mounting a second one. See `src/state/browsing.ts`.
 */
export default function GalleryRoute() {
  const { view } = useView();
  const { timeline, reload } = useCachedTimeline(undefined, view);
  const actions = useTrashActions(undefined, reload, undefined, view);
  useBrowsing(timeline);
  return <Grid timeline={timeline} actions={actions} />;
}
