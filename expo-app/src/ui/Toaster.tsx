import { Feather } from '@expo/vector-icons';
import { useEffect, useState } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { color, radius, space } from '../theme';
import { Text } from './Text';
import { closeToast, subscribeToasts, type Toast } from './toast';

/**
 * The notices, drawn.
 *
 * Mounted once in the root layout, above the router's stack, so a notice
 * outlives the screen that caused it — which it has to: a delete reloads the
 * timeline it happened on, and the undo it offers has to survive the grid
 * being torn down. That is the same reason the browser's `<Toaster>` sits in
 * `layout.tsx` rather than in a page.
 *
 * They stack from the bottom, above the tab bar, because that is the half of
 * the screen a thumb is already near.
 */
export function Toaster({ bottom = 0 }: { bottom?: number }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const insets = useSafeAreaInsets();

  useEffect(() => subscribeToasts(setToasts), []);

  if (toasts.length === 0) return null;

  return (
    <View
      // `box-none` rather than `none`: the toasts themselves have an action to
      // press, but the empty column they are stacked in must not swallow taps
      // meant for the grid behind it.
      pointerEvents="box-none"
      style={[styles.stack, { bottom: insets.bottom + bottom + space.md }]}
    >
      {toasts.map((toast) => (
        <ToastRow key={toast.id} toast={toast} />
      ))}
    </View>
  );
}

function ToastRow({ toast }: { toast: Toast }) {
  const tone = toast.type === 'error' ? 'destructive' : toast.type === 'success' ? 'success' : 'muted';

  return (
    <View style={[styles.toast, toast.type === 'error' && styles.error]}>
      <Feather
        name={toast.type === 'error' ? 'alert-triangle' : toast.type === 'success' ? 'check' : 'info'}
        size={16}
        color={color[tone === 'muted' ? 'mutedForeground' : tone]}
        style={styles.icon}
      />
      <View style={styles.body}>
        <Text variant="body" numberOfLines={2}>
          {toast.title}
        </Text>
        {toast.description !== undefined && (
          <Text variant="small" tone="muted" numberOfLines={3}>
            {toast.description}
          </Text>
        )}
      </View>

      {toast.action && (
        <Pressable
          accessibilityRole="button"
          onPress={() => {
            // Closed first: the action reloads whatever it is undoing, and a
            // notice still on screen afterwards would offer to undo it again.
            closeToast(toast.id);
            toast.action?.onPress();
          }}
          hitSlop={8}
        >
          <Text variant="small" tone="primary" style={styles.action}>
            {toast.action.label}
          </Text>
        </Pressable>
      )}

      <Pressable
        accessibilityRole="button"
        accessibilityLabel="Dismiss"
        onPress={() => closeToast(toast.id)}
        hitSlop={8}
      >
        <Feather name="x" size={15} color={color.faint} />
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  stack: {
    position: 'absolute',
    left: space.md,
    right: space.md,
    gap: space.sm,
    zIndex: 40,
  },
  toast: {
    flexDirection: 'row',
    alignItems: 'flex-start',
    gap: space.sm,
    backgroundColor: color.popover,
    borderWidth: 1,
    borderColor: color.border,
    borderRadius: radius.lg,
    paddingHorizontal: space.md,
    paddingVertical: space.md,
  },
  error: { borderColor: color.destructive },
  icon: { marginTop: 2 },
  body: { flex: 1, gap: 2 },
  action: { fontWeight: '600' },
});
