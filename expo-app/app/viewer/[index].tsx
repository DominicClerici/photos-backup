import { Redirect, useLocalSearchParams } from 'expo-router';

import { useBrowsed } from '../../src/state/browsing';
import { Viewer } from '../../src/viewer';

/**
 * One photograph, at a position in whatever is being browsed.
 *
 * A position and not an id, which is the browser's choice too — `useViewer`
 * says the path is what is being browsed and the query is where in it. Here the
 * path carries both, and it can, because a route is pushed rather than typed:
 * whoever pushed it is the screen holding the timeline, and it has published it
 * on the way past.
 *
 * That publication is the whole of the wiring, and `src/state/browsing.ts` says
 * why it is not a context. A route is a sibling of the tab it was opened from
 * rather than a child of it, so a viewer that wanted the gallery's timeline
 * through React would have to mount its own — a second day table and a spinner
 * over a photograph that is already on screen.
 *
 * Nothing published means nobody is browsing: a deep link straight to this
 * route with no gallery behind it, which is a redirect rather than a state to
 * draw.
 */
export default function ViewerRoute() {
  const { index } = useLocalSearchParams<{ index: string }>();
  const browsed = useBrowsed();

  if (!browsed) return <Redirect href="/" />;

  const at = Number.parseInt(index ?? '', 10);
  return (
    <Viewer
      timeline={browsed.timeline}
      at={Number.isFinite(at) ? at : 0}
      // Whether this photograph came out of the vault, which travels with the
      // timeline rather than being asked for here: a route cannot see the
      // filter the grid was built from. What it buys is that a decrypted
      // preview is never written to disk — see src/vault.
      sealed={browsed.sealed}
    />
  );
}
