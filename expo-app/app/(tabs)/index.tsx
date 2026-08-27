import { useTimeline } from '@photobackup/core/react';

import { Grid } from '../../src/grid';

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
 * No filter and no view: this is the library, newest first. The sort-and-filter
 * control and the collections that would pass a filter are Phase 5. The third
 * argument — the offline store — is the seam WEB_TO_MOBILE § 3.6 asks to be
 * built now and filled in Phase 6; passing nothing is exactly what the browser
 * does, and is why nothing about the hook's behaviour changed when it grew.
 */
export default function GalleryRoute() {
  const timeline = useTimeline();
  return <Grid timeline={timeline} />;
}
