import type { CollectionFilter } from '@photobackup/core';
import { Redirect, useLocalSearchParams } from 'expo-router';

import { CollectionView } from '../../../../src/collections';

/** The three kinds of collection, and the only three this route will draw. */
const KINDS = ['albums', 'people', 'categories'] as const;

/**
 * One album, one person, one category — the same route the browser has, and
 * deliberately: § 3.3 asks for the Next routes one for one, so a link to
 * /collections/albums/<id> means the same thing typed into either app.
 *
 * Inside the Collections tab's own stack rather than at the root, so the Back
 * gesture comes out of a collection into the list of them instead of out of the
 * tab altogether.
 */
export default function CollectionRoute() {
  const { kind, value } = useLocalSearchParams<{ kind: string; value: string }>();

  const known = (KINDS as readonly string[]).includes(kind ?? '');
  if (!known || !value) return <Redirect href="/collections" />;

  const filter = { kind, value } as CollectionFilter;
  // Keyed so that walking from one collection to another rebuilds the timeline
  // rather than reusing a store whose every index means a different photograph.
  return <CollectionView key={`${kind}/${value}`} filter={filter} />;
}
