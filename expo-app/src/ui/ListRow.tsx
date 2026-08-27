import { Feather } from '@expo/vector-icons';
import { Children, type ReactNode } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { Text } from './Text';

/**
 * A group of rows on one raised surface, hairlines between them.
 *
 * The browser draws this as a `<ul>` with `rounded-xl border bg-card` and a
 * `border-t` on every row but the first. There is no `:first-child` here, so
 * the separator is decided by this component rather than by each row — which is
 * also what keeps a list from drawing a rule above a row that is conditionally
 * absent.
 */
export function RowList({ children }: { children: ReactNode }) {
  const rows = Children.toArray(children).filter(Boolean);
  return (
    <View style={styles.list}>
      {rows.map((row, i) => (
        <View key={i} style={i > 0 ? styles.divided : undefined}>
          {row}
        </View>
      ))}
    </View>
  );
}

/**
 * One row of such a list: something on the left, a name, and what there is of
 * it on the right.
 *
 * `value` is a string rather than a number for the reason `Count` is: "not
 * counted yet" and "empty" are different things, and only one of them is worth
 * saying — so a row with nothing to report is given nothing rather than a zero.
 */
export function ListRow({
  leading,
  label,
  value,
  trailing,
  onPress,
  disabled = false,
}: {
  leading?: ReactNode;
  label: string;
  value?: string;
  /** Drawn instead of the chevron, for a row that says something else. */
  trailing?: ReactNode;
  onPress?: () => void;
  /**
   * A row that is drawn but cannot be gone into.
   *
   * Nothing uses it since Phase 6 gave the two vault buckets and Recently
   * Deleted the screens they were waiting for. It stays because the shape it
   * exists for keeps recurring: a list that pretended a destination did not
   * exist would be a worse account of the archive than one that says "not yet".
   */
  disabled?: boolean;
}) {
  const body = (
    <View style={[styles.row, disabled && styles.dim]}>
      {leading}
      <Text variant="body" numberOfLines={1} style={styles.label}>
        {label}
      </Text>
      {value === undefined ? null : (
        <Text variant="small" tone="faint">
          {value}
        </Text>
      )}
      {trailing ??
        (onPress && !disabled ? (
          <Feather name="chevron-right" size={16} color={color.faint} />
        ) : null)}
    </View>
  );

  if (!onPress || disabled) return body;

  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      onPress={onPress}
      style={({ pressed }) => pressed && styles.pressed}
    >
      {body}
    </Pressable>
  );
}

/** The square a category's cover sits in, and the tinted glyph over it. */
export const ROW_ICON_SIZE = 44;

export const styles = StyleSheet.create({
  list: {
    backgroundColor: color.card,
    borderRadius: radius.xl,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    overflow: 'hidden',
  },
  divided: { borderTopWidth: StyleSheet.hairlineWidth, borderTopColor: color.border },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    paddingHorizontal: space.md,
    paddingVertical: space.sm + 2,
    minHeight: 64,
  },
  label: { flex: 1 },
  dim: { opacity: 0.55 },
  pressed: { backgroundColor: color.accent },
});
