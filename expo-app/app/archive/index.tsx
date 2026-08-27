import { BucketView } from '../../src/vault';

/**
 * Its own route rather than /collections/archive, for the reason /trash has its
 * own: a collection is a slice of the library, and this is what has left it —
 * encrypted, out of every album, and unreadable without a password.
 *
 * The two buckets get two routes rather than one parameterised one because they
 * are two destinations somebody links to and deep-links into. /vault/archive
 * would be an implementation detail wearing a URL, and the browser's routes are
 * the ones this app is mapping one for one.
 */
export default function ArchiveRoute() {
  return <BucketView bucket="archive" />;
}
