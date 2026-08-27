import type { CollectionFilter } from '@photobackup/core';
import { Redirect, useLocalSearchParams } from 'expo-router';

import { BucketTimeline } from '../../../src/vault';

/** The three kinds of collection, and the only three this route will draw. */
const KINDS = ['albums', 'people', 'categories'] as const;

/** One collection inside Hidden. See the Archive's own, which says why. */
export default function HiddenCollectionRoute() {
  const { kind, value } = useLocalSearchParams<{ kind: string; value: string }>();

  const known = (KINDS as readonly string[]).includes(kind ?? '');
  if (!known || !value) return <Redirect href="/hidden" />;

  return (
    <BucketTimeline
      key={`${kind}/${value}`}
      bucket="hidden"
      within={{ kind, value } as CollectionFilter}
    />
  );
}
