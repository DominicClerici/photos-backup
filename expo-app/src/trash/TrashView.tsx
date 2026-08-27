import type { TimelineFilter } from '@photobackup/core';
import { useTrashActions, useView } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { router } from 'expo-router';
import { Pressable, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { useCachedTimeline } from '../gallery/cache';
import { Grid } from '../grid';
import { useBrowsing } from '../state/browsing';
import { color, space } from '../theme';
import { Text } from '../ui';

/**
 * A module constant rather than a literal in the body, so its identity is
 * stable across renders — the actions memoise on it, and a fresh object every
 * render would rebuild them every render.
 */
const TRASH: TimelineFilter = { kind: 'trash' };

/**
 * Recently Deleted, browsed exactly as the library is.
 *
 * The same grid, the same viewer, the same zoom, the same paging, over the same
 * timeline read with one predicate flipped. What differs is only what a
 * selection can do: here the destructive action is final and there is a restore
 * beside it, which `useTrashActions` already knows from the filter.
 *
 * Which is the whole argument for making the trash a scope rather than a place.
 * A separate "deleted items" screen would be a second grid to keep in step with
 * the first, and it would be the worse of the two, because nobody looks at it
 * often enough to notice it rotting.
 */
export function TrashView() {
  const insets = useSafeAreaInsets();
  const { view } = useView();
  const { timeline, reload } = useCachedTimeline(TRASH, view);
  const actions = useTrashActions(TRASH, reload, undefined, view);
  useBrowsing(timeline);

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
          Recently Deleted
        </Text>
        <Text variant="small" tone="faint">
          {timeline.total.toLocaleString()}
        </Text>
      </View>

      {/* The retention is the whole promise this screen makes, so it is on the
          screen rather than behind a tap. */}
      <Text variant="caption" tone="faint" style={styles.retention}>
        Deleted for good after 365 days.
      </Text>

      <Grid
        timeline={timeline}
        filter={TRASH}
        actions={actions}
        immersive={false}
        empty="Nothing deleted."
      />
    </View>
  );
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
  retention: { paddingHorizontal: space.lg, paddingBottom: space.sm },
  pressed: { opacity: 0.6 },
});
