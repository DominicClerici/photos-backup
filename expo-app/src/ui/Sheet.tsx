import { useEffect, useState } from 'react';
import { Pressable, ScrollView, StyleSheet, useWindowDimensions, View } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { color, radius, space } from '../theme';
import { Text } from './Text';

const OPEN_MS = 240;
const CLOSE_MS = 180;

/** How far down, or how fast, a drag has to go before releasing it closes. */
const DISMISS_DISTANCE = 90;
const DISMISS_VELOCITY = 700;

/**
 * The tallest a sheet gets, as a fraction of the screen.
 *
 * The browser's metadata panel is a sidebar on a wide screen and a bottom sheet
 * under 700px, capped at 55% there. A phone has no sidebar to fall back to and
 * the panel is the only place some of these fields exist, so it is given a
 * little more room than the browser's narrow case and scrolls past it.
 */
const MOST = 0.68;

/**
 * A panel that comes up from the bottom edge, over whatever is behind it.
 *
 * Written for the viewer's metadata panel, which is its only caller and the
 * reason it exists at all — `src/ui/index.ts` has said since Phase 2 that a
 * primitive written before its first use is a guess at an API rather than one.
 * So this is exactly what that panel needs and nothing more: a scrim that
 * dismisses, a grab handle that drags, a title, and a body that scrolls when
 * there is more of it than there is room.
 *
 * Not a `Modal`. The viewer is already a full-screen route above the tab bar,
 * so a second window would only add a second animation going the other way —
 * the reason `grid/Peek.tsx` gives for needing one does not apply here.
 *
 * The sheet is drawn over the photograph rather than shrinking it into the
 * space above, which is the one thing it does differently from the browser's
 * panel. There the photo is given back the width the sidebar takes; here the
 * photo can be zoomed and panned, and moving the stage under a live transform
 * would fight the gesture holding it.
 */
export function Sheet({
  open,
  onClose,
  title,
  children,
}: {
  open: boolean;
  onClose: () => void;
  /** Shown beside the handle. Wraps rather than truncates — it is a filename. */
  title?: string;
  children: React.ReactNode;
}) {
  const { height } = useWindowDimensions();
  const insets = useSafeAreaInsets();

  // Held past the moment the caller closes it, so the sheet slides back down
  // instead of vanishing. Anything the body is showing stays mounted for the
  // length of the animation, which is what makes the exit look like one thing
  // leaving rather than two.
  const [mounted, setMounted] = useState(open);
  const shown = useSharedValue(0);
  const drag = useSharedValue(0);

  useEffect(() => {
    if (open) {
      setMounted(true);
      drag.value = 0;
      shown.value = withTiming(1, { duration: OPEN_MS, easing: Easing.out(Easing.cubic) });
      return;
    }
    shown.value = withTiming(0, { duration: CLOSE_MS, easing: Easing.in(Easing.cubic) }, (done) => {
      if (done) runOnJS(setMounted)(false);
    });
  }, [open, shown, drag]);

  const pan = Gesture.Pan()
    // Vertical only: a horizontal swipe on a sheet over a pager belongs to
    // neither, and letting this claim it would make the panel wobble whenever
    // somebody meant to change photograph and missed.
    .activeOffsetY([-10, 10])
    .failOffsetX([-20, 20])
    .onUpdate((e) => {
      // Downwards freely, upwards barely: there is nothing above the top of a
      // sheet, and a rubber band says so better than a hard stop.
      drag.value = e.translationY > 0 ? e.translationY : e.translationY / 6;
    })
    .onEnd((e) => {
      if (e.translationY > DISMISS_DISTANCE || e.velocityY > DISMISS_VELOCITY) {
        runOnJS(onClose)();
        return;
      }
      drag.value = withTiming(0, { duration: 180, easing: Easing.out(Easing.cubic) });
    });

  const scrim = useAnimatedStyle(() => ({ opacity: shown.value * 0.55 }));
  const panel = useAnimatedStyle(() => ({
    transform: [{ translateY: (1 - shown.value) * height + drag.value }],
  }));

  if (!mounted) return null;

  return (
    <View style={StyleSheet.absoluteFill} pointerEvents="box-none">
      <Animated.View style={[StyleSheet.absoluteFill, styles.scrim, scrim]}>
        <Pressable
          style={StyleSheet.absoluteFill}
          onPress={onClose}
          accessibilityRole="button"
          accessibilityLabel="Close the panel"
        />
      </Animated.View>

      <Animated.View
        style={[styles.panel, { maxHeight: height * MOST, paddingBottom: insets.bottom + space.lg }, panel]}
      >
        <GestureDetector gesture={pan}>
          <View style={styles.grip}>
            <View style={styles.handle} />
            {title ? (
              <Text variant="title" style={styles.title}>
                {title}
              </Text>
            ) : null}
          </View>
        </GestureDetector>

        <ScrollView
          contentContainerStyle={styles.body}
          showsVerticalScrollIndicator={false}
          keyboardShouldPersistTaps="handled"
        >
          {children}
        </ScrollView>
      </Animated.View>
    </View>
  );
}

const styles = StyleSheet.create({
  scrim: { backgroundColor: '#000000' },
  panel: {
    position: 'absolute',
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: color.card,
    borderTopLeftRadius: radius.xl,
    borderTopRightRadius: radius.xl,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
  },
  // The whole header is the handle, not just the bar drawn in it: a four-point
  // target is a fiddly thing to catch on a photograph somebody is holding a
  // phone over.
  grip: { paddingTop: space.sm, paddingHorizontal: space.lg, paddingBottom: space.md },
  handle: {
    alignSelf: 'center',
    width: 36,
    height: 4,
    borderRadius: radius.pill,
    backgroundColor: color.border,
  },
  title: { marginTop: space.md },
  body: { paddingHorizontal: space.lg, paddingBottom: space.md, gap: space.lg },
});
