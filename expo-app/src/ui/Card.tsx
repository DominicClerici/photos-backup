import { StyleSheet, View, type ViewProps } from 'react-native';

import { color, radius, space } from '../theme';
import { Text } from './Text';

/**
 * A titled group of things, on the app's raised surface.
 *
 * `App.tsx`'s `Section`, given the theme's colours and a name that says what it
 * is rather than where it sat. The gap is fixed rather than a prop: every card
 * in this app spaces its children the same, and one that did not would be the
 * odd one out rather than the special case.
 */
export function Card({
  title,
  children,
  style,
  ...rest
}: ViewProps & { title?: string; children: React.ReactNode }) {
  return (
    <View {...rest} style={[styles.card, style]}>
      {title !== undefined && (
        <Text variant="caption" tone="muted" style={styles.title}>
          {title}
        </Text>
      )}
      {children}
    </View>
  );
}

/** Things side by side, centred on each other. The commonest layout there is. */
export function Row({ style, ...rest }: ViewProps) {
  return <View {...rest} style={[styles.row, style]} />;
}

/**
 * Three numbers across the width of a card, each under its label.
 *
 * `Count` takes a string rather than a number because every one of these is
 * already through a formatter — `formatBytes`, `formatCount`, `formatLastBackup`
 * — and an em dash is a legitimate value when the server has not answered.
 */
export function Counts({ children }: { children: React.ReactNode }) {
  return <View style={styles.counts}>{children}</View>;
}

export function Count({
  label,
  value,
  tone = 'default',
}: {
  label: string;
  value: string;
  tone?: 'default' | 'success' | 'destructive';
}) {
  return (
    <View style={styles.count}>
      <Text variant="display" tone={tone}>
        {value}
      </Text>
      <Text variant="caption" tone="faint">
        {label}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: color.card,
    borderRadius: radius.lg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    padding: space.md,
    gap: space.sm,
  },
  title: {
    fontWeight: '600',
    textTransform: 'uppercase',
    letterSpacing: 0.6,
  },
  row: { flexDirection: 'row', gap: space.sm, alignItems: 'center' },
  counts: { flexDirection: 'row', justifyContent: 'space-between', marginTop: space.xs },
  count: { alignItems: 'center', flex: 1 },
});
