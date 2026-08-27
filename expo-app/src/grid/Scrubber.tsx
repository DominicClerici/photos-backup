import { useMemo } from 'react';
import { StyleSheet } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  scrollTo,
  useAnimatedReaction,
  useAnimatedStyle,
  useDerivedValue,
  useSharedValue,
  withDelay,
  withSequence,
  withTiming,
  type AnimatedRef,
  type SharedValue,
} from 'react-native-reanimated';

import { color, radius, space } from '../theme';
import { Text } from '../ui';
import { clamp, valueAt, type Series } from './geometry';

const HANDLE_HEIGHT = 44;
const RAIL_WIDTH = 40;
/** Quiet time after the last scroll before the rail gets out of the way. */
const IDLE_MS = 1400;

/**
 * Jumping to a date, as a rail rather than a calendar.
 *
 * The browser answers this with a month calendar inside its filter popover,
 * which is the right control for a pointer and the wrong one for a thumb. What
 * a phone wants is the gesture every photo app has: drag the edge and the
 * archive pours past, with the date under your thumb the whole way.
 *
 * It is exact for the same reason the calendar is: the scroll extent is the day
 * table's `totalHeight`, known before a single photograph has been fetched, so
 * a drag to two thirds of the way down lands on the photograph that is two
 * thirds of the way down — not on the last thing that happened to load. The
 * date beside the handle is `dayAt` read at the offset the drag is writing,
 * which arrives here as `label` because the scroll events the drag causes go
 * through exactly the same path an ordinary scroll does.
 */
export function Scrubber({
  scroller,
  scrollY,
  z,
  heights,
  chrome,
  viewport,
  label,
  top,
  bottom,
}: {
  scroller: AnimatedRef<Animated.ScrollView>;
  scrollY: SharedValue<number>;
  z: SharedValue<number>;
  /** The board's height at each zoom level. See grid/geometry. */
  heights: Series;
  /** The padding above and below the board, which the scroll extent includes. */
  chrome: number;
  viewport: number;
  label: string;
  top: number;
  bottom: number;
}) {
  const railHeight = useSharedValue(0);
  const dragging = useSharedValue(false);
  const shown = useSharedValue(0);
  /**
   * How far into a drag the handle is, 0–1, for the lift and the date bubble.
   *
   * A value of its own rather than `withTiming(dragging.value ? … )` read inside
   * the styles below. Those styles re-evaluate on every frame of scroll, and an
   * animation started inside one starts afresh each time it is read — so it
   * would be permanently on its first frame and never actually move.
   */
  const lifted = useSharedValue(0);

  /** How far the scroll can go, which changes as the zoom does. */
  const limit = useDerivedValue(() =>
    Math.max(0, valueAt(heights, z.value) + chrome - viewport),
  );

  /** Where the handle sits, 0–1 down the rail. */
  const progress = useDerivedValue(() =>
    limit.value <= 0 ? 0 : clamp(scrollY.value / limit.value, 0, 1),
  );

  // Surfaces whenever the grid moves and gets out of the way when it stops,
  // for the reason the browser's zoom slider does: it is a readout most of the
  // time and a control occasionally, and a photograph is what the screen is for.
  useAnimatedReaction(
    () => scrollY.value,
    () => {
      // A sequence rather than a delayed fade, because a delay holds the value
      // it starts from: the rail is hidden at rest, so "wait, then go to zero"
      // would leave it hidden forever. It has to be brought up first, and the
      // whole sequence restarted on every frame of scroll — which is what makes
      // the countdown run from the last movement rather than from the first.
      shown.value = dragging.value
        ? withTiming(1, { duration: 100 })
        : withSequence(
            withTiming(1, { duration: 120 }),
            withDelay(IDLE_MS, withTiming(0, { duration: 400 })),
          );
    },
  );

  /** Where the handle was when the finger came down, which a drag is relative to. */
  const travel = useSharedValue(0);

  const drag = useMemo(
    () =>
      Gesture.Pan()
        .onBegin(() => {
          dragging.value = true;
          shown.value = withTiming(1, { duration: 80 });
          lifted.value = withTiming(1, { duration: 120 });
          travel.value = progress.value * Math.max(0, railHeight.value - HANDLE_HEIGHT);
        })
        .onUpdate((e) => {
          const span = Math.max(1, railHeight.value - HANDLE_HEIGHT);
          const at = clamp(travel.value + e.translationY, 0, span);
          // Writing the offset is the whole of it: the scroll events this
          // causes go through the grid's own handler, so the date beside the
          // handle and the tiles under it are found the same way an ordinary
          // scroll finds them.
          scrollTo(scroller, 0, (at / span) * limit.value, false);
        })
        .onFinalize(() => {
          dragging.value = false;
          lifted.value = withTiming(0, { duration: 160 });
          shown.value = withDelay(IDLE_MS, withTiming(0, { duration: 400 }));
        }),
    [dragging, lifted, limit, progress, railHeight, scroller, shown, travel],
  );

  const railStyle = useAnimatedStyle(() => ({
    opacity: shown.value,
    // `box-none` rather than `auto`, because React Native hit-tests a view's
    // whole box whether or not anything is drawn in it — an invisible forty-
    // point strip down the right edge would otherwise swallow every press on
    // the last column of photographs. Only the handle is meant to be touchable,
    // and only while the rail is up.
    pointerEvents: shown.value > 0.1 ? 'box-none' : 'none',
  }));

  const handleStyle = useAnimatedStyle(() => ({
    transform: [
      { translateY: progress.value * Math.max(0, railHeight.value - HANDLE_HEIGHT) },
      { scale: 1 + lifted.value * 0.1 },
    ],
  }));

  const bubbleStyle = useAnimatedStyle(() => ({
    opacity: lifted.value,
    transform: [{ translateY: progress.value * Math.max(0, railHeight.value - HANDLE_HEIGHT) }],
  }));

  if (viewport === 0) return null;

  return (
    <Animated.View
      style={[styles.rail, { top, bottom }, railStyle]}
      onLayout={(e) => {
        railHeight.value = e.nativeEvent.layout.height;
      }}
    >
      <Animated.View style={[styles.bubble, bubbleStyle]} pointerEvents="none">
        <Text variant="caption" numberOfLines={1}>
          {label}
        </Text>
      </Animated.View>

      <GestureDetector gesture={drag}>
        <Animated.View style={[styles.handle, handleStyle]} hitSlop={space.md}>
          <Animated.View style={styles.grip} />
          <Animated.View style={styles.grip} />
        </Animated.View>
      </GestureDetector>
    </Animated.View>
  );
}

const styles = StyleSheet.create({
  rail: { position: 'absolute', right: 0, width: RAIL_WIDTH },
  handle: {
    position: 'absolute',
    right: space.xs,
    width: 26,
    height: HANDLE_HEIGHT,
    borderRadius: radius.pill,
    backgroundColor: color.card,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    alignItems: 'center',
    justifyContent: 'center',
    gap: 3,
  },
  grip: { width: 10, height: 1.5, borderRadius: 1, backgroundColor: color.mutedForeground },
  bubble: {
    position: 'absolute',
    right: RAIL_WIDTH,
    height: HANDLE_HEIGHT,
    justifyContent: 'center',
    borderRadius: radius.pill,
    backgroundColor: color.card,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    paddingHorizontal: space.md,
  },
});
