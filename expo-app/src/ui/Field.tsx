import { StyleSheet, TextInput, View, type TextInputProps } from 'react-native';

import { color, mono, radius, space, text } from '../theme';
import { Text } from './Text';

/**
 * Somewhere to type.
 *
 * `placeholderTextColor` is set here and nowhere else: React Native does not
 * inherit it from a style, so every bare `TextInput` in the app was repeating a
 * hex, which is exactly the thing the theme exists to stop.
 */
export function Field({
  label,
  code,
  align,
  style,
  ...rest
}: TextInputProps & {
  /** A label to the left, for the fields that sit on a row with one. */
  label?: string;
  /** Monospaced and letterspaced — a pairing code, and nothing else. */
  code?: boolean;
  align?: 'left' | 'right';
}) {
  const input = (
    <TextInput
      {...rest}
      placeholderTextColor={color.faint}
      style={[
        styles.input,
        // A field with a label beside it sizes to its content, not to half the
        // row: the label is the thing that should absorb the slack, so that
        // "Newest items to consider" stays on one line and 0 does not get a
        // box half a screen wide.
        label !== undefined && styles.bounded,
        code && styles.code,
        align === 'right' && styles.right,
        rest.editable === false && styles.readOnly,
        style,
      ]}
    />
  );

  if (label === undefined) return input;

  return (
    <View style={styles.labelled}>
      <Text variant="small" tone="muted" style={styles.label}>
        {label}
      </Text>
      {input}
    </View>
  );
}

const styles = StyleSheet.create({
  input: {
    flex: 1,
    backgroundColor: color.secondary,
    borderWidth: 1,
    borderColor: color.input,
    color: color.foreground,
    borderRadius: radius.md,
    paddingHorizontal: space.md,
    paddingVertical: 10,
    ...text.body,
  },
  bounded: { flex: 0, minWidth: 96 },
  code: { fontFamily: mono, letterSpacing: 2 },
  right: { textAlign: 'right' },
  readOnly: { color: color.mutedForeground },
  labelled: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  label: { flex: 1 },
});
