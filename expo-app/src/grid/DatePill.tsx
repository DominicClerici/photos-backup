import { StyleSheet } from 'react-native';
import Animated, { useAnimatedStyle, withTiming } from 'react-native-reanimated';

import { color, radius, space } from '../theme';
import { Text } from '../ui';

/**
 * The date of whatever is under the top of the screen.
 *
 * The browser's floating pill, and the same rule about when it shows: only once
 * its own heading has scrolled away, so it never sits directly above a heading
 * saying the same thing. The label comes from `dayAt` in core, which reads it
 * out of the day table rather than out of anything that has been fetched — so
 * it is right while scrolling through a stretch of the archive that is still
 * placeholders.
 */
export function DatePill({
  label,
  visible,
  top,
}: {
  label: string;
  visible: boolean;
  top: number;
}) {
  const style = useAnimatedStyle(() => ({
    opacity: withTiming(visible ? 1 : 0, { duration: 150 }),
  }));

  return (
    <Animated.View style={[styles.pill, { top }, style]} pointerEvents="none">
      <Text variant="caption" numberOfLines={1}>
        {label}
      </Text>
    </Animated.View>
  );
}

const styles = StyleSheet.create({
  pill: {
    position: 'absolute',
    left: space.md,
    borderRadius: radius.pill,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    // Opaque rather than translucent: there is no backdrop-filter to lean on
    // here, and a half-transparent label over a wall of photographs is a label
    // nobody can read.
    backgroundColor: color.card,
    paddingHorizontal: space.md,
    paddingVertical: space.xs + 1,
  },
});
