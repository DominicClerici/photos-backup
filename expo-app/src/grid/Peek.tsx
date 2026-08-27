import {
  BASE_THUMB_SIZE,
  liveThumbVariant,
  media,
  thumbVariant,
  THUMB_SIZES,
  type ThumbSize,
  type TimelineItem,
} from '@photobackup/core';
import { BlurView } from 'expo-blur';
import * as Haptics from 'expo-haptics';
import { Image } from 'expo-image';
import { useVideoPlayer, VideoView } from 'expo-video';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Modal, Pressable, StyleSheet, useWindowDimensions, View } from 'react-native';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { color, radius, space } from '../theme';
import { Text } from '../ui';

const OPEN_MS = 220;
const CLOSE_MS = 160;
/** The margin the enlarged photograph keeps from the sides of the screen. */
const MARGIN = space.xxl;
/** The largest stored rendition, and the only place its extra pixels show. */
const LARGE: ThumbSize = THUMB_SIZES[THUMB_SIZES.length - 1];

export interface PeekTarget {
  item: TimelineItem;
  /** Where the tile is on screen right now, so the photograph grows out of it. */
  from: { x: number; y: number; size: number };
}

/**
 * A photograph held for a moment, out of the grid.
 *
 * The iOS Photos gesture: hold a tile and it lifts off the wall, the wall goes
 * soft behind it, and a Live Photo plays the three seconds it carries. Tap
 * anywhere to put it back.
 *
 * This is the phone's answer to the hover the browser's Tile has. There, a
 * pointer resting on a tile starts its motion after 120ms — a gesture a phone
 * does not have, and one whose touch equivalent is a trap: `pointerenter` fires
 * for a tap and `pointerleave` often does not, which is a video left playing
 * under a finger that has moved on. A hold is unambiguous, and it is the same
 * hold the multi-select grows out of. Phase 5 hangs Select, Add to album,
 * Archive, Hide and Delete underneath this, which is why the photograph lifts
 * towards the top of the screen rather than into the middle of it: the room
 * below is where those go.
 *
 * The still stays mounted underneath the video for the browser's reason — the
 * dissolve is then between two pictures of the same moment, and never lets the
 * backdrop show through a frame that has not arrived.
 */
export function Peek({
  target,
  onClose,
}: {
  target: PeekTarget | null;
  onClose: () => void;
}) {
  const { width, height } = useWindowDimensions();
  const insets = useSafeAreaInsets();
  const open = useSharedValue(0);
  const playing = useSharedValue(0);

  // Held past the moment the parent lets go, so the photograph can shrink back
  // into the square it came from instead of vanishing out of a blur.
  const [held, setHeld] = useState<PeekTarget | null>(null);
  const [broken, setBroken] = useState(false);

  useEffect(() => {
    if (!target) return;
    setHeld(target);
    setBroken(false);
    playing.value = 0;
    void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Medium);
    open.value = withTiming(1, { duration: OPEN_MS, easing: Easing.out(Easing.cubic) });
  }, [target, open, playing]);

  const dismiss = useCallback(() => {
    open.value = withTiming(0, { duration: CLOSE_MS, easing: Easing.in(Easing.cubic) }, (done) => {
      if (done) runOnJS(setHeld)(null);
    });
    onClose();
  }, [onClose, open]);

  const item = held?.item ?? null;

  // A square, because a square is what the grid holds: the stored thumbnail is
  // a centre crop, and the full frame is the viewer's business in Phase 4.
  const side = Math.min(width - MARGIN * 2, height * 0.55);
  const to = useMemo(
    () => ({ x: (width - side) / 2, y: insets.top + space.xl, size: side }),
    [width, side, insets.top],
  );
  const from = held?.from ?? to;

  const motion = useMemo(
    () => (item && item.live === 'ready' ? media(item.id, liveThumbVariant(LARGE)) : null),
    [item],
  );
  const player = useVideoPlayer(motion, (p) => {
    // Muted is not a preference: this is three seconds of motion attached to a
    // photograph, and it has never carried sound anybody wanted.
    p.muted = true;
    p.loop = false;
    p.play();
  });

  // Width and height rather than a scale, so the corner radius is the same
  // fourteen pixels whatever square the photograph grew out of. One node
  // laying itself out per frame is not the cost a screenful of tiles would be.
  const cardStyle = useAnimatedStyle(() => {
    const t = open.value;
    return {
      width: from.size + (to.size - from.size) * t,
      height: from.size + (to.size - from.size) * t,
      transform: [
        { translateX: from.x + (to.x - from.x) * t },
        { translateY: from.y + (to.y - from.y) * t },
      ],
    };
  });

  const scrimStyle = useAnimatedStyle(() => ({ opacity: open.value }));
  const videoStyle = useAnimatedStyle(() => ({ opacity: playing.value }));
  const captionStyle = useAnimatedStyle(() => ({
    opacity: open.value,
    transform: [{ translateY: (1 - open.value) * space.md }],
  }));

  if (!item) return null;

  // A library ingested before the 512 rendition existed has only the base one
  // until a backfill runs, so a 404 here is a gap in what is stored rather than
  // a missing photograph, and the next-best file is a better answer than none.
  const still = media(item.id, thumbVariant(broken ? BASE_THUMB_SIZE : LARGE));

  return (
    // A modal rather than a view in the grid, because the grid is a tab screen
    // and the floating tab bar is that navigator's sibling to it — drawn after
    // it, and so over anything the screen puts on top of itself. A blurred wall
    // with the tab bar sitting brightly on top of it is not a wall going soft.
    // `animationType="none"`: the photograph's own growth out of its square is
    // the animation, and a modal sliding up underneath it would be a second one
    // going the other way.
    <Modal
      transparent
      statusBarTranslucent
      visible
      animationType="none"
      onRequestClose={dismiss}
    >
      <Animated.View style={[StyleSheet.absoluteFill, scrimStyle]} pointerEvents="none">
        <BlurView intensity={38} tint="dark" style={StyleSheet.absoluteFill} />
        <View style={styles.dim} />
      </Animated.View>

      <Pressable
        style={StyleSheet.absoluteFill}
        onPress={dismiss}
        accessibilityRole="button"
        accessibilityLabel="Close"
      />

      <Animated.View style={[styles.card, cardStyle]} pointerEvents="none">
        <Image
          style={StyleSheet.absoluteFill}
          source={still}
          contentFit="cover"
          cachePolicy="memory-disk"
          transition={0}
          onError={() => setBroken(true)}
        />
        {motion ? (
          <Animated.View style={[StyleSheet.absoluteFill, videoStyle]}>
            <VideoView
              style={StyleSheet.absoluteFill}
              player={player}
              contentFit="cover"
              nativeControls={false}
              onFirstFrameRender={() => {
                playing.value = withTiming(1, { duration: 180 });
              }}
            />
          </Animated.View>
        ) : null}
      </Animated.View>

      <Animated.View
        style={[styles.caption, { top: to.y + side }, captionStyle]}
        pointerEvents="none"
      >
        <Text variant="small" tone="muted">
          {new Date(item.taken_at).toLocaleString(undefined, {
            weekday: 'short',
            day: 'numeric',
            month: 'long',
            year: 'numeric',
            hour: 'numeric',
            minute: '2-digit',
          })}
        </Text>
      </Animated.View>
    </Modal>
  );
}

const styles = StyleSheet.create({
  dim: { position: 'absolute', top: 0, left: 0, right: 0, bottom: 0, backgroundColor: 'rgba(8,8,10,0.5)' },
  card: {
    position: 'absolute',
    top: 0,
    left: 0,
    overflow: 'hidden',
    borderRadius: radius.xl,
    backgroundColor: color.tile,
  },
  caption: {
    position: 'absolute',
    left: 0,
    right: 0,
    alignItems: 'center',
    paddingTop: space.lg,
  },
});
