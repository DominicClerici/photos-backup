import { Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { Text } from './Text';

/**
 * A small rounded label, on or off.
 *
 * The browser's `FilterPill` and `SelectionPill` are this shape, and so is the
 * row of sample sizes in the shared-album diagnostics — a set of choices small
 * enough that a picker would be more machinery than the question deserves.
 * `onPress` is optional because a pill is also just a label saying what is
 * currently true.
 */
export function Pill({
  label,
  on = false,
  onPress,
  disabled = false,
  tone = 'default',
}: {
  label: string;
  on?: boolean;
  onPress?: () => void;
  disabled?: boolean;
  tone?: 'default' | 'warning' | 'destructive' | 'success';
}) {
  const body = (
    <Text variant="small" tone={on ? 'default' : tone === 'default' ? 'muted' : tone}>
      {label}
    </Text>
  );

  if (!onPress) {
    return <View style={[styles.pill, on && styles.on]}>{body}</View>;
  }

  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ selected: on, disabled }}
      onPress={onPress}
      disabled={disabled}
      style={({ pressed }) => [
        styles.pill,
        on && styles.on,
        disabled && styles.off,
        pressed && !disabled && styles.pressed,
      ]}
    >
      {body}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  pill: {
    paddingHorizontal: space.md,
    paddingVertical: 6,
    borderRadius: radius.pill,
    borderWidth: 1,
    borderColor: color.border,
    backgroundColor: color.secondary,
  },
  on: { borderColor: color.primary, backgroundColor: color.accent },
  off: { opacity: 0.5 },
  pressed: { opacity: 0.72 },
});
