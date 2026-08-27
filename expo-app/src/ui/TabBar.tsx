import { Feather } from '@expo/vector-icons';
import { router } from 'expo-router';
import type { BottomTabBarProps } from 'expo-router/tabs';
import { useCallback, useEffect, useRef, useState } from 'react';
import { Animated, Pressable, StyleSheet, View, type LayoutChangeEvent } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { color, radius, space } from '../theme';
import { Text } from './Text';

/** What each route is called and what it is drawn as. */
const LABELS: Record<string, { label: string; icon: React.ComponentProps<typeof Feather>['name'] }> =
  {
    index: { label: 'Gallery', icon: 'image' },
    collections: { label: 'Collections', icon: 'folder' },
    backup: { label: 'Backup', icon: 'upload-cloud' },
  };

/** Where the highlight sits, in pixels along the row. */
interface Pill {
  x: number;
  w: number;
}

/**
 * The section switcher: a floating bar over whatever screen is on.
 *
 * The browser's `TabBar` in shape and in behaviour. The tabs themselves never
 * change size — only their colour does — and the highlight behind them is a
 * single element that slides and stretches from one to the next. Anything that
 * changed a tab's own width (a bolder weight on the active one, a label that
 * appears only when selected) would have the row re-flowing underneath the very
 * animation meant to track it.
 *
 * The first placement is where the highlight *starts*, so it is drawn there
 * rather than travelled to: otherwise every launch flies it in from the left
 * edge. `placed` is what distinguishes the two.
 *
 * There is no blur behind it. `expo-blur` would be a sixth native dependency
 * for one surface, and a solid card at this contrast is the same picture.
 *
 * The search button is beside the row rather than in it, and it is a button
 * rather than a fourth tab because a search is a question rather than a
 * destination — the same reading the browser's bar makes. Its own circle, so
 * that nothing about the tabs moves when it appears.
 */
export function TabBar({ state, navigation }: BottomTabBarProps) {
  const insets = useSafeAreaInsets();
  const [pills, setPills] = useState<Record<number, Pill>>({});
  const [placed, setPlaced] = useState(false);

  const x = useRef(new Animated.Value(0)).current;
  const w = useRef(new Animated.Value(0)).current;

  const active = state.index;
  const current = pills[active];

  const measure = useCallback(
    (index: number) => (event: LayoutChangeEvent) => {
      const { x: left, width } = event.nativeEvent.layout;
      setPills((previous) => {
        const known = previous[index];
        if (known && known.x === left && known.w === width) return previous;
        return { ...previous, [index]: { x: left, w: width } };
      });
    },
    []
  );

  useEffect(() => {
    if (!current) return;

    if (!placed) {
      // The first placement is where the highlight *starts*. Set rather than
      // animated, and only once a tab has actually been measured — otherwise
      // every launch flies it in from the left edge.
      x.setValue(current.x);
      w.setValue(current.w);
      setPlaced(true);
      return;
    }

    // Overshoot-free: it leaves fast and settles slowly, which is what makes a
    // short slide read as deliberate rather than springy. `useNativeDriver` is
    // false because `width` is a layout property and the native driver only
    // handles transforms and opacity.
    Animated.parallel([
      Animated.timing(x, { toValue: current.x, duration: 280, useNativeDriver: false }),
      Animated.timing(w, { toValue: current.w, duration: 280, useNativeDriver: false }),
    ]).start();
  }, [current, placed, x, w]);

  return (
    <View
      // `box-none`: the bar is a control, the space around it is the grid.
      pointerEvents="box-none"
      style={[styles.dock, { bottom: insets.bottom + space.md }]}
    >
      <Pressable
        accessibilityRole="button"
        accessibilityLabel="Search"
        onPress={() => router.push('/search')}
        style={({ pressed }) => [styles.search, pressed && styles.pressed]}
      >
        <Feather name="search" size={18} color={color.mutedForeground} />
      </Pressable>

      <View style={styles.row}>
        {current && (
          <Animated.View
            pointerEvents="none"
            style={[styles.highlight, { transform: [{ translateX: x }], width: w }]}
          />
        )}

        {state.routes.map((route, index) => {
          const meta = LABELS[route.name];
          if (!meta) return null;
          const on = index === active;

          return (
            <Pressable
              key={route.key}
              accessibilityRole="tab"
              accessibilityState={{ selected: on }}
              accessibilityLabel={meta.label}
              onLayout={measure(index)}
              onPress={() => {
                const event = navigation.emit({
                  type: 'tabPress',
                  target: route.key,
                  canPreventDefault: true,
                });
                if (!on && !event.defaultPrevented) {
                  navigation.navigate(route.name, route.params);
                }
              }}
              style={styles.tab}
            >
              <Feather
                name={meta.icon}
                size={18}
                color={on ? color.primary : color.mutedForeground}
              />
              <Text
                variant="small"
                tone={on ? 'default' : 'muted'}
                numberOfLines={1}
                style={styles.label}
              >
                {meta.label}
              </Text>
            </Pressable>
          );
        })}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  dock: {
    position: 'absolute',
    left: space.sm,
    right: space.sm,
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: space.sm,
    zIndex: 30,
  },
  // The tabs shrink before the search button does. On a narrow phone the
  // labels are what give, and a tab that has lost a letter is still a tab you
  // can hit; a search button that has been squeezed to nothing is not.
  row: {
    flexShrink: 1,
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.xs,
    padding: 6,
    borderRadius: radius.pill,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.card,
    // A floating control needs to read as floating. iOS honours the shadow
    // props; the elevation is what Android reads instead.
    shadowColor: '#000',
    shadowOpacity: 0.4,
    shadowRadius: 12,
    shadowOffset: { width: 0, height: 6 },
    elevation: 8,
  },
  highlight: {
    position: 'absolute',
    top: 6,
    bottom: 6,
    left: 0,
    borderRadius: radius.pill,
    backgroundColor: color.accent,
  },
  tab: {
    height: 40,
    flexShrink: 1,
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    borderRadius: radius.pill,
  },
  label: { fontWeight: '500', flexShrink: 1 },
  search: {
    width: 52,
    height: 52,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.pill,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.card,
    shadowColor: '#000',
    shadowOpacity: 0.4,
    shadowRadius: 12,
    shadowOffset: { width: 0, height: 6 },
    elevation: 8,
  },
  pressed: { opacity: 0.7 },
});
