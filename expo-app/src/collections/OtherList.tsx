import { Feather } from '@expo/vector-icons';
import { StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { ListRow, ROW_ICON_SIZE, RowList, Text } from '../ui';

type Glyph = React.ComponentProps<typeof Feather>['name'];

/**
 * The rows that are less a slice of the library than a place a photograph is
 * put: out of the timeline, out of sight, or on its way out altogether.
 *
 * The row is CategoryList's, minus the cover. A category always has a
 * photograph that can stand for it — the server never sends an empty one —
 * whereas these are fixed destinations that exist whether or not anything is in
 * them, so the glyph carries the row on its own.
 *
 * Four rows, and the first two are not the same thing despite both saying
 * something like "archive".
 *
 * **Archived** is Google's flag, imported. It is a category like any other — a
 * predicate over `assets.archived`, linking to the same filtered timeline every
 * other category links to — and it says what Google Photos was told, years ago,
 * about photographs that are otherwise entirely ordinary members of this
 * library. Nothing about it is encrypted and nothing about it is a decision
 * made here.
 *
 * **Archive** is this archive's own, added in Phase 11. A photograph in it is
 * encrypted on disk, out of every album and category, and unreadable without a
 * password. The two are not related and the labels are the only thing they have
 * in common, which is why the older one keeps the past tense it was imported
 * with and the new one takes the plain noun.
 *
 * All four lead somewhere now. The last three are root routes rather than
 * screens in this tab, because each of them is a full-width grid of its own —
 * see `app/_layout.tsx`.
 */
const ENTRIES: { key: string; label: string; icon: Glyph }[] = [
  { key: 'archived', label: 'Archived', icon: 'inbox' },
  { key: 'archive', label: 'Archive', icon: 'archive' },
  { key: 'hidden', label: 'Hidden', icon: 'eye-off' },
  { key: 'trash', label: 'Recently Deleted', icon: 'trash-2' },
];

/** The two rows whose count is a fact about the vault rather than about a slice. */
const SEALED = new Set(['archive', 'hidden']);

/**
 * @param counts How much is in each entry, by key. A key with no count draws no
 * number rather than a zero, because "not counted yet" and "empty" are
 * different things and only one of them is worth saying.
 *
 * The vault's two rows exercise that distinction for a stronger reason than the
 * others: while the vault is locked the server does not send those counts at
 * all. How much somebody has hidden is a fact about what they hid, and a row
 * reading "Hidden — 41" would give it away to anyone who glanced at the screen.
 *
 * @param onOpen where the one live row leads.
 */
export function OtherList({
  counts,
  onOpen,
}: {
  counts?: Record<string, number | undefined>;
  onOpen: (key: string) => void;
}) {
  return (
    <RowList>
      {ENTRIES.map(({ key, label, icon }) => {
        const count = counts?.[key];
        // A locked vault does not say how much is in it — the server withholds
        // those two counts rather than sending zero — so the row says which of
        // the two silences this is. "Locked" is the honest word for it, and it
        // is a fact about the vault rather than about what somebody hid.
        const shut = SEALED.has(key) && count === undefined;
        return (
          <ListRow
            key={key}
            label={label}
            value={count === undefined ? undefined : count.toLocaleString()}
            onPress={() => onOpen(key)}
            trailing={
              shut ? (
                <Text variant="caption" tone="faint">
                  Locked
                </Text>
              ) : undefined
            }
            leading={
              <View style={styles.badge}>
                <Feather name={icon} size={18} color={color.foreground} />
              </View>
            }
          />
        );
      })}
    </RowList>
  );
}

const styles = StyleSheet.create({
  badge: {
    width: ROW_ICON_SIZE,
    height: ROW_ICON_SIZE,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.lg,
    backgroundColor: color.tile,
    marginVertical: space.xs,
  },
});
