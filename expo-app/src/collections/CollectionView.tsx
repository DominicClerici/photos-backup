import { fetchAlbum, type Album, type CollectionFilter } from '@photobackup/core';
import { useTrashActions, useView } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { router } from 'expo-router';
import { useEffect, useState } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { albumKey, recall, remember, useCachedTimeline } from '../gallery/cache';
import { Grid } from '../grid';
import { useBrowsing } from '../state/browsing';
import { color, space } from '../theme';
import { Text } from '../ui';
import { categoryLabel } from './CategoryList';

/**
 * One collection, browsed exactly as the library is.
 *
 * The grid, the viewer, the zoom, the paging and the selection are the
 * gallery's own, given a filtered timeline: a collection is a narrower query,
 * not a different kind of thing, and anything built separately here would be a
 * second grid to keep in step with the first.
 *
 * The one thing it has that the library screen does not is a header, and so the
 * grid below it does not run under the status bar — which is why `Grid` takes
 * the whole of the space left rather than the whole screen. The floating date
 * and the scrubber sit inside that space and need no telling.
 */
export function CollectionView({ filter }: { filter: CollectionFilter }) {
  const insets = useSafeAreaInsets();
  const { view } = useView();
  const { timeline, reload } = useCachedTimeline(filter, view);
  useBrowsing(timeline);

  const heading = useHeading(filter);
  // The filter goes with the actions because a position is a position *in this
  // collection*: index 2 of an album is not index 2 of the library.
  //
  // The name goes with them too, and only because of the notice: "removed from
  // Iceland 2025" needs a word the filter does not carry.
  const actions = useTrashActions(filter, reload, heading.album, view);

  return (
    <View style={styles.root}>
      <View style={[styles.header, { paddingTop: insets.top + space.sm }]}>
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Back to collections"
          onPress={() => router.back()}
          hitSlop={10}
          style={({ pressed }) => pressed && styles.pressed}
        >
          <Feather name="chevron-left" size={22} color={color.mutedForeground} />
        </Pressable>

        <Text variant="title" numberOfLines={1} style={styles.title}>
          {heading.title}
        </Text>
        <Text variant="small" tone="faint">
          {timeline.total.toLocaleString()}
        </Text>
      </View>

      {/* Beside the count would be a second row on most screens for a field
          most albums do not have, so it goes under the header and only when
          there is one. */}
      {heading.description ? (
        <Text variant="small" tone="faint" numberOfLines={2} style={styles.description}>
          {heading.description}
        </Text>
      ) : null}

      <Grid
        timeline={timeline}
        filter={filter}
        actions={actions}
        immersive={false}
        empty="Nothing in here."
      />
    </View>
  );
}

/**
 * What to call this collection, and what else its heading has to say.
 *
 * Two of the three kinds already carry their name in the route, and only an
 * album — addressed by uuid — has to be asked about. That lookup is its own
 * endpoint rather than a scan of the collections index, so opening one album
 * does not cost a count of every other one.
 *
 * And it is remembered, because it is the one part of this screen that a cached
 * timeline cannot supply. A grid of photographs under the word "Album" is a
 * worse screen than the same grid under "Iceland 2025", and on a phone out of
 * reach of the archive that would be every album, every time.
 */
function useHeading(filter: CollectionFilter): {
  title: string;
  description?: string;
  /** The album's own name, when this is an album. What a notice calls it. */
  album?: string;
} {
  const [album, setAlbum] = useState<Album | null>(null);

  useEffect(() => {
    setAlbum(null);
    if (filter.kind !== 'albums') return;
    const abort = new AbortController();
    const key = albumKey(filter.value);
    let answered = false;

    void recall<Album>(key).then((held) => {
      if (!held || answered || abort.signal.aborted) return;
      setAlbum(held);
    });

    fetchAlbum(filter.value, abort.signal)
      .then((fresh) => {
        if (abort.signal.aborted) return;
        answered = true;
        setAlbum(fresh);
        remember(key, fresh);
      })
      // A heading that says "Album" is a worse screen than one that says
      // "Iceland 2025", and a better one than no photographs at all.
      .catch(() => {});
    return () => abort.abort();
  }, [filter.kind, filter.value]);

  switch (filter.kind) {
    case 'albums':
      return {
        title: album?.title ?? 'Album',
        description: album?.description,
        album: album?.title,
      };
    case 'people':
      return { title: filter.value };
    case 'categories':
      return { title: categoryLabel(filter.value) };
  }
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: color.background },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingBottom: space.sm,
  },
  title: { flex: 1 },
  description: { paddingHorizontal: space.lg, paddingBottom: space.sm },
  pressed: { opacity: 0.6 },
});
