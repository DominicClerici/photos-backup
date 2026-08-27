import { Text as RNText, StyleSheet, View, type TextProps } from 'react-native';

import { color, mono as monoFace, space, text } from '../theme';

export type TextVariant = keyof typeof text;
export type TextTone =
  | 'default'
  | 'muted'
  | 'faint'
  | 'primary'
  | 'success'
  | 'warning'
  | 'destructive';

/**
 * Every word on screen, sized and coloured from the theme.
 *
 * It exists so that nothing else has to name a font size or a hex, which is the
 * one rule `web/AGENTS.md` states about colour and the only one that survives
 * the crossing to a platform with no utility classes. `variant` is what it is,
 * `tone` is what it means.
 */
export function Text({
  variant = 'body',
  tone = 'default',
  mono,
  style,
  ...rest
}: TextProps & { variant?: TextVariant; tone?: TextTone; mono?: boolean }) {
  return <RNText {...rest} style={[text[variant], tones[tone], mono && styles.mono, style]} />;
}

/**
 * A heading over a group of rows, with the hairline above it.
 *
 * Carried across from `App.tsx`'s `subheading` rather than reinvented: an
 * uppercase, letterspaced label under a rule is how this app has always divided
 * a card up, and it reads the same on a settings page as it did on the one
 * screen. `trailing` is for the "all / none" links that sit beside one — they
 * go on the row so the rule is drawn above both rather than through one.
 */
export function Subheading({
  children,
  trailing,
}: {
  children: React.ReactNode;
  trailing?: React.ReactNode;
}) {
  return (
    <View style={styles.subheadingRow}>
      <RNText style={styles.subheading}>{children}</RNText>
      {trailing}
    </View>
  );
}

const tones = StyleSheet.create({
  default: { color: color.foreground },
  muted: { color: color.mutedForeground },
  faint: { color: color.faint },
  primary: { color: color.primary },
  success: { color: color.success },
  warning: { color: color.warning },
  destructive: { color: color.destructive },
});

const styles = StyleSheet.create({
  mono: { fontFamily: monoFace },
  subheadingRow: {
    flexDirection: 'row',
    alignItems: 'baseline',
    gap: space.md,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
    paddingTop: space.sm,
    marginTop: space.xs,
  },
  subheading: {
    flex: 1,
    color: color.faint,
    fontSize: 11,
    textTransform: 'uppercase',
    letterSpacing: 1,
  },
});
