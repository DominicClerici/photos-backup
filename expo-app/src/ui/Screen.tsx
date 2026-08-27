import { Feather } from '@expo/vector-icons';
import { Pressable, ScrollView, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { color, space } from '../theme';
import { Text } from './Text';

/**
 * How much room the floating tab bar takes out of the bottom of every screen.
 *
 * The bar is drawn over the content rather than beside it — see
 * `app/(tabs)/_layout.tsx` — so nothing else would know to stop short of it.
 * Exported because the viewer and the toaster both have to clear the same bar.
 */
export const TAB_BAR_CLEARANCE = 76;

/**
 * A scrolling page with a title and, optionally, one control beside it.
 *
 * Every screen in the app that is a list of cards is this. The gallery is not —
 * it draws to the edges and puts its own chrome on top — which is why the
 * scrolling is here rather than in the tab layout.
 */
export function Screen({
  title,
  action,
  children,
  scrolls = true,
}: {
  title: string;
  action?: { icon: React.ComponentProps<typeof Feather>['name']; label: string; onPress: () => void };
  children: React.ReactNode;
  scrolls?: boolean;
}) {
  const insets = useSafeAreaInsets();

  const header = (
    <View style={styles.header}>
      <Text variant="display" style={styles.title}>
        {title}
      </Text>
      {action && (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={action.label}
          onPress={action.onPress}
          hitSlop={10}
          style={({ pressed }) => pressed && styles.pressed}
        >
          <Feather name={action.icon} size={20} color={color.mutedForeground} />
        </Pressable>
      )}
    </View>
  );

  if (!scrolls) {
    return (
      <View style={[styles.screen, { paddingTop: insets.top }]}>
        {header}
        <View style={styles.grow}>{children}</View>
      </View>
    );
  }

  return (
    <View style={[styles.screen, { paddingTop: insets.top }]}>
      {header}
      <ScrollView
        contentContainerStyle={[
          styles.content,
          { paddingBottom: insets.bottom + TAB_BAR_CLEARANCE + space.lg },
        ]}
        keyboardDismissMode="on-drag"
        keyboardShouldPersistTaps="handled"
      >
        {children}
      </ScrollView>
    </View>
  );
}

/**
 * What a screen says when there is nothing in it yet.
 *
 * Phase 2 leaves Gallery and Collections empty on purpose, and an empty screen
 * that says nothing is indistinguishable from one that failed to load.
 */
export function Empty({
  icon,
  title,
  children,
}: {
  icon: React.ComponentProps<typeof Feather>['name'];
  title: string;
  children?: React.ReactNode;
}) {
  return (
    <View style={styles.empty}>
      <Feather name={icon} size={30} color={color.faint} />
      <Text variant="title" tone="muted">
        {title}
      </Text>
      {children}
    </View>
  );
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: color.background },
  grow: { flex: 1 },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    paddingHorizontal: space.lg,
    paddingTop: space.sm,
    paddingBottom: space.sm,
  },
  title: { flex: 1 },
  pressed: { opacity: 0.6 },
  content: { paddingHorizontal: space.lg, gap: space.md },
  empty: {
    flex: 1,
    alignItems: 'center',
    justifyContent: 'center',
    gap: space.sm,
    paddingHorizontal: space.xl,
    paddingBottom: TAB_BAR_CLEARANCE,
  },
});
