import { Feather } from '@expo/vector-icons';
import { ActivityIndicator, Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { Text } from './Text';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'destructive';

/**
 * The one control that does something.
 *
 * `busy` rather than a caller-swapped label: a button that says "Pairing…" and
 * a button that is disabled are the same state, and having one prop own both
 * means no screen can show a spinner on a button somebody can still press.
 */
export function Button({
  label,
  onPress,
  variant = 'secondary',
  icon,
  busy = false,
  disabled = false,
  grow = false,
}: {
  label: string;
  onPress: () => void;
  variant?: ButtonVariant;
  icon?: React.ComponentProps<typeof Feather>['name'];
  busy?: boolean;
  disabled?: boolean;
  /** Fills the row it is in, for the one button on a line of two. */
  grow?: boolean;
}) {
  const off = disabled || busy;

  return (
    <Pressable
      accessibilityRole="button"
      accessibilityState={{ disabled: off, busy }}
      onPress={onPress}
      disabled={off}
      style={({ pressed }) => [
        styles.base,
        surfaces[variant],
        grow && styles.grow,
        // A ghost button has no surface to grey out, so switching it to one
        // when it is disabled would make it appear rather than dim.
        off && (variant === 'ghost' ? null : styles.off),
        pressed && !off && styles.pressed,
      ]}
    >
      <View style={styles.row}>
        {busy ? (
          <ActivityIndicator size="small" color={off ? color.faint : labelColor[variant]} />
        ) : icon ? (
          <Feather name={icon} size={15} color={off ? color.faint : labelColor[variant]} />
        ) : null}
        <Text
          variant="body"
          style={[styles.label, { color: off ? color.faint : labelColor[variant] }]}
        >
          {label}
        </Text>
      </View>
    </Pressable>
  );
}

/**
 * The primary button is the app's blue, which is a light colour, so its label
 * is the background rather than the foreground — the same inversion
 * `--primary-foreground` makes in the browser.
 */
const labelColor: Record<ButtonVariant, string> = {
  primary: color.primaryForeground,
  secondary: color.foreground,
  ghost: color.mutedForeground,
  destructive: color.destructive,
};

const surfaces = StyleSheet.create({
  primary: { backgroundColor: color.primary },
  secondary: { backgroundColor: color.secondary, borderWidth: 1, borderColor: color.border },
  ghost: { backgroundColor: 'transparent' },
  destructive: {
    backgroundColor: 'transparent',
    borderWidth: 1,
    borderColor: color.destructive,
  },
});

const styles = StyleSheet.create({
  base: {
    borderRadius: radius.md,
    paddingHorizontal: space.lg,
    paddingVertical: 11,
    alignItems: 'center',
    justifyContent: 'center',
  },
  row: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  label: { fontWeight: '600' },
  grow: { flex: 1 },
  // A disabled button keeps its shape and loses its colour, rather than fading
  // out: a half-transparent control over a dark card reads as a rendering
  // glitch, and the label still has to be legible enough to say why it is off.
  off: { backgroundColor: color.secondary, borderColor: color.border },
  pressed: { opacity: 0.72 },
});
