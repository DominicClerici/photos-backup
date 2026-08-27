import { TrashView } from '../src/trash';

/**
 * Its own route rather than /collections/trash, because it is not one: a
 * collection is a slice of the library, and this is what has left it. The row
 * that leads here has always been under Other for the same reason.
 *
 * At the root rather than inside the Collections tab — WEB_TO_MOBILE § 3.3 puts
 * it there, and the shape agrees with the reading: a full-width grid with no
 * tab bar over it, which is what the library's own screen is too.
 */
export default function TrashRoute() {
  return <TrashView />;
}
