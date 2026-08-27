import { Feather } from '@expo/vector-icons';
import { useEffect, useState, type ReactNode } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { Sheet } from './Sheet';
import { Text } from './Text';

type Glyph = React.ComponentProps<typeof Feather>['name'];

export interface Action {
  key: string;
  label: string;
  icon: Glyph;
  tone?: 'default' | 'destructive';
  /**
   * Two presses rather than one, the first of which only says what the second
   * will do.
   *
   * The browser arms its destructive controls instead of raising a dialog, on
   * the grounds that a second click in the same place is a cheaper second
   * decision than a modal across the screen. A phone has the stronger version
   * of that argument: the sheet is already the modal, and stacking an alert on
   * top of it would be two surfaces to dismiss for one act.
   */
  armed?: boolean;
  disabled?: boolean;
  /** Why it is off, said beside it. A disabled row with no reason is a bug. */
  note?: string;
  onPress?: () => void;
}

/**
 * A list of things that can be done, up from the bottom edge.
 *
 * What the browser draws as a context menu or as the panel above the selection
 * pill. Neither shape survives the crossing — there is no pointer to right-
 * click with and no room to float a 240-point panel over a floating tab bar —
 * so both become the one gesture a phone already has for "what can I do with
 * this", which is a sheet.
 *
 * The armed state is held here rather than per row so that arming one disarms
 * the rest: two live confirmations in a list of five is a sheet where the wrong
 * thing is one tap away.
 */
export function ActionSheet({
  open,
  onClose,
  title,
  description,
  actions,
  footer,
}: {
  open: boolean;
  onClose: () => void;
  title?: string;
  /** A line under the title, for what the actions below are about to affect. */
  description?: string;
  actions: Action[];
  /** Drawn under the list — the Done button, the small print. */
  footer?: ReactNode;
}) {
  const [armed, setArmed] = useState<string | null>(null);

  // Reopened on whatever was armed last otherwise, which is a destructive row
  // sitting one tap from firing on a sheet somebody has only just opened.
  useEffect(() => {
    if (!open) setArmed(null);
  }, [open]);

  return (
    <Sheet open={open} onClose={onClose} title={title}>
      {description ? (
        <Text variant="small" tone="muted">
          {description}
        </Text>
      ) : null}

      <View style={styles.list}>
        {actions.map((action) => (
          <ActionRow
            key={action.key}
            action={action}
            armed={armed === action.key}
            onArm={() => setArmed(action.key)}
            onDisarm={() => setArmed(null)}
          />
        ))}
      </View>

      {footer}
    </Sheet>
  );
}

function ActionRow({
  action,
  armed,
  onArm,
  onDisarm,
}: {
  action: Action;
  armed: boolean;
  onArm: () => void;
  onDisarm: () => void;
}) {
  const { label, icon, tone = 'default', disabled, note, onPress } = action;
  const destructive = tone === 'destructive';
  const tint = disabled ? color.faint : destructive ? color.destructive : color.foreground;

  const press = () => {
    if (action.armed && !armed) {
      onArm();
      return;
    }
    onDisarm();
    onPress?.();
  };

  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityState={{ disabled: !!disabled }}
      disabled={disabled}
      onPress={press}
      style={({ pressed }) => [
        styles.row,
        armed && styles.armedRow,
        pressed && !disabled && styles.pressed,
      ]}
    >
      <Feather name={armed ? 'alert-triangle' : icon} size={17} color={tint} />
      <Text variant="body" numberOfLines={1} style={[styles.label, { color: tint }]}>
        {armed ? `Tap again to ${label.toLowerCase()}` : label}
      </Text>
      {note && !armed ? (
        <Text variant="caption" tone="faint">
          {note}
        </Text>
      ) : null}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  list: { gap: space.sm },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    minHeight: 48,
    paddingHorizontal: space.md,
    borderRadius: radius.lg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.secondary,
  },
  armedRow: { borderColor: color.destructive },
  label: { flex: 1 },
  pressed: { opacity: 0.72 },
});
