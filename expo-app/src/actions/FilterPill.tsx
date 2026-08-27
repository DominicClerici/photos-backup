import {
  describeView,
  FACET_LABEL,
  facetsOn,
  isDefault,
  pickSort,
  SORT_LABEL,
  toggleFacet,
  type Facet,
  type Facets,
  type View,
} from '@photobackup/core';
import { useView } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { Pressable, StyleSheet, View as Box } from 'react-native';

import { color, radius, space } from '../theme';
import { Sheet, Subheading, Text } from '../ui';

type Glyph = React.ComponentProps<typeof Feather>['name'];

/**
 * The orders, in the row they are drawn in.
 *
 * The last two are marked rather than explained: ordering by length is a
 * question about videos, and the row says so before it is tapped instead of the
 * filter appearing to move by itself afterwards. See core's `pickSort`.
 */
const SORTS: { key: View['sort']; icon: Glyph; note?: string }[] = [
  { key: 'newest', icon: 'arrow-down' },
  { key: 'oldest', icon: 'arrow-up' },
  { key: 'longest', icon: 'chevrons-down', note: 'Videos' },
  { key: 'shortest', icon: 'chevrons-up', note: 'Videos' },
];

const FACET_ICON: Record<Facet, Glyph> = {
  all: 'grid',
  photos: 'image',
  videos: 'video',
  favorites: 'star',
  unalbumed: 'folder-minus',
};

/**
 * The sort-and-filter control, and the sheet it opens.
 *
 * It draws nothing at all unless a grid is on screen, which is what keeps it
 * off the collections screen and the backup tab — and off the search results,
 * where the order *is* the ranking and a control offering "Newest" would either
 * do nothing or throw the answer away. See core's `useViewScope`, which the
 * search grid deliberately passes null.
 *
 * A circle while the grid is the one it opens as, and a pill saying what has
 * been done to it otherwise. Which is `describeView`'s whole job: a pill
 * reading "Newest" on every ordinary grid would be saying nothing while taking
 * up the room that says something.
 */
export function FilterPill() {
  const { view, setView, facets, grid } = useView();
  const [open, setOpen] = useState(false);

  // A grid that has gone away takes the sheet with it, rather than leaving it
  // open over whatever replaced it.
  useEffect(() => {
    if (!grid) setOpen(false);
  }, [grid]);

  if (!grid) return null;

  const label = describeView(view);
  const plain = isDefault(view);

  return (
    <>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={plain ? 'Sort and filter' : `Sorted and filtered: ${label}`}
        onPress={() => setOpen(true)}
        style={({ pressed }) => [
          styles.pill,
          !plain && styles.lit,
          pressed && styles.pressed,
        ]}
      >
        <Feather
          name="sliders"
          size={16}
          color={plain ? color.mutedForeground : color.primary}
        />
        {plain ? null : (
          <Text variant="small" numberOfLines={1} style={styles.pillLabel}>
            {label}
          </Text>
        )}
      </Pressable>

      <FilterSheet
        open={open}
        onClose={() => setOpen(false)}
        view={view}
        facets={facets}
        onChange={setView}
      />
    </>
  );
}

/**
 * The order, then what to leave out — the two questions in the order somebody
 * reaches for them.
 *
 * The browser folds the filters away behind a disclosure because most of the
 * time the answer is "all of them". A sheet has the room and no hover to hint
 * with, so both are simply here, and a filter that is on is one glance rather
 * than one tap plus a glance.
 */
function FilterSheet({
  open,
  onClose,
  view,
  facets,
  onChange,
}: {
  open: boolean;
  onClose: () => void;
  view: View;
  facets: Facets;
  onChange: (next: View) => void;
}) {
  const on = facetsOn(view);
  // "All" is not a filter but the absence of every other one, which is why it
  // is in the same row: the control is a set of toggles, and the way to say
  // "none of these" in a set of toggles is a button that turns the rest off.
  const shown: Facet[] = [
    'all',
    ...(facets.media ? (['photos', 'videos'] as Facet[]) : []),
    ...(facets.favorites ? (['favorites'] as Facet[]) : []),
    ...(facets.unalbumed ? (['unalbumed'] as Facet[]) : []),
  ];

  return (
    <Sheet open={open} onClose={onClose} title="Sort & filter">
      <Box style={styles.group}>
        <Subheading>Order</Subheading>
        {SORTS.map(({ key, icon, note }) => {
          const picked = view.sort === key;
          return (
            <Pressable
              key={key}
              accessibilityRole="radio"
              accessibilityState={{ selected: picked }}
              onPress={() => onChange(pickSort(view, key, facets))}
              style={({ pressed }) => [styles.row, pressed && styles.pressed]}
            >
              <Feather
                name={icon}
                size={17}
                color={picked ? color.primary : color.mutedForeground}
              />
              <Text variant="body" style={styles.rowLabel}>
                {SORT_LABEL[key]}
              </Text>
              {note ? (
                <Text variant="caption" tone="faint">
                  {note}
                </Text>
              ) : null}
              {picked ? <Feather name="check" size={16} color={color.primary} /> : null}
            </Pressable>
          );
        })}
      </Box>

      <Box style={styles.group}>
        <Subheading>Show</Subheading>
        <Box style={styles.facets}>
          {shown.map((facet) => {
            const lit = on.includes(facet);
            return (
              <Pressable
                key={facet}
                accessibilityRole="button"
                accessibilityState={{ selected: lit }}
                onPress={() => onChange(toggleFacet(view, facet))}
                style={({ pressed }) => [
                  styles.facet,
                  lit && styles.facetOn,
                  pressed && styles.pressed,
                ]}
              >
                <Feather
                  name={FACET_ICON[facet]}
                  size={14}
                  color={lit ? color.primary : color.mutedForeground}
                />
                <Text variant="small" tone={lit ? 'default' : 'muted'}>
                  {FACET_LABEL[facet]}
                </Text>
              </Pressable>
            );
          })}
        </Box>
      </Box>
    </Sheet>
  );
}

const styles = StyleSheet.create({
  pill: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    maxWidth: 220,
    height: 40,
    paddingHorizontal: space.md,
    borderRadius: radius.pill,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.card,
    shadowColor: '#000',
    shadowOpacity: 0.4,
    shadowRadius: 12,
    shadowOffset: { width: 0, height: 6 },
    elevation: 8,
  },
  lit: { borderColor: color.primary },
  pillLabel: { flexShrink: 1 },
  pressed: { opacity: 0.72 },

  group: { gap: space.sm },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    minHeight: 44,
    paddingHorizontal: space.md,
    borderRadius: radius.lg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.secondary,
  },
  rowLabel: { flex: 1 },
  facets: { flexDirection: 'row', flexWrap: 'wrap', gap: space.sm },
  facet: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingVertical: space.sm,
    borderRadius: radius.pill,
    borderWidth: 1,
    borderColor: color.border,
    backgroundColor: color.secondary,
  },
  facetOn: { borderColor: color.primary, backgroundColor: color.accent },
});
