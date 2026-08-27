import { Collections } from '../../../src/collections';

/**
 * Albums, people, categories, and the four places a photograph is put rather
 * than a slice of the library it belongs to.
 *
 * The whole index is one request — albums and people are counted in tens, not
 * thousands — so there is nothing here that loads as it scrolls.
 */
export default function CollectionsRoute() {
  return <Collections />;
}
