import type { CollectionFilter } from '@photobackup/core';
import { Redirect, useLocalSearchParams } from 'expo-router';

import { BucketTimeline } from '../../../src/vault';

/** The three kinds of collection, and the only three this route will draw. */
const KINDS = ['albums', 'people', 'categories'] as const;

/**
 * One collection inside the Archive: /archive/albums/<uuid>,
 * /archive/people/<name>, /archive/categories/<key>.
 *
 * Deliberately the same three kinds and the same shape as
 * /collections/[kind]/[value]. A hidden photograph is still in the albums it
 * was in and still has the people in it that it had — all of that went into the
 * sealed document with it — so a bucket has real collections rather than a flat
 * pile, and browsing them is the same act with the same URL grammar.
 */
export default function ArchiveCollectionRoute() {
  const { kind, value } = useLocalSearchParams<{ kind: string; value: string }>();

  const known = (KINDS as readonly string[]).includes(kind ?? '');
  if (!known || !value) return <Redirect href="/archive" />;

  // Keyed so that walking from one collection to another rebuilds the timeline
  // rather than reusing a store whose every index means a different photograph.
  return (
    <BucketTimeline
      key={`${kind}/${value}`}
      bucket="archive"
      within={{ kind, value } as CollectionFilter}
    />
  );
}
