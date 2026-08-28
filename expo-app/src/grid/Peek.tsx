import {
  BASE_THUMB_SIZE,
  liveThumbVariant,
  media,
  thumbVariant,
  THUMB_SIZES,
  type ThumbSize,
  type TimelineItem,
} from '@photobackup/core';
import { Feather } from '@expo/vector-icons';
import { BlurView } from 'expo-blur';
import * as Haptics from 'expo-haptics';
import { Image } from 'expo-image';
import { useVideoPlayer, VideoView } from 'expo-video';
import { useEffect, useMemo, useState } from 'react';
import { Modal, Pressable, StyleSheet, useWindowDimensions, View } from 'react-native';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import type { MediaCache } from '../gallery/cache';
import { color, radius, space } from '../theme';
import { Text, type Action } from '../ui';
import { RELEASE_MS } from './Tile';

/**
 * The lift, and the way back.
 *
 * The open is a fifth of a second because the photograph is already most of the
 * way there — the hold has drawn the tile back to `PRESS_SCALE` and the card
 * starts from exactly that square, so this animates the rest of one continuous
 * movement rather than beginning a new one. Going back is quicker still: the
 * decision has been made and the grid is what somebody wants to see.
 */
const OPEN_MS = 200;
const CLOSE_MS = RELEASE_MS;
/** The margin the enlarged photograph keeps from the sides of the screen. */
const MARGIN = space.xxl;
/** The largest stored rendition, and the only place its extra pixels show. */
const LARGE: ThumbSize = THUMB_SIZES[THUMB_SIZES.length - 1];

export interface PeekTarget {
  item: TimelineItem;
  /** Which position in the timeline it is, which is what Select picks. */
  index: number;
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
 * under a finger that has moved on. A hold is unambiguous, and Phase 5 hung
 * Select, Add to album, Archive, Hide and Delete underneath it — which is why
 * the photograph lifts towards the top of the screen rather than into the
 * middle of it. The room below was left for them.
 *
 * The hold means the same thing whether or not the grid is picking: it peeks.
 * What changes is the first row under the photograph, which says Select while
 * the grid is browsing and Deselect once this photograph is in the selection.
 * That row is one of the two ways into selection mode, the other being the
 * pill; the drag that picks a run of them is inside that mode and never a way
 * into it. See the pan in `Grid`.
 *
 * The still stays mounted underneath the video for the browser's reason — the
 * dissolve is then between two pictures of the same moment, and never lets the
 * backdrop show through a frame that has not arrived.
 */
export function Peek({
  target,
  actions = [],
  cache = 'memory-disk',
  onClose,
}: {
  target: PeekTarget | null;
  /**
   * How long the held photograph's bytes may be kept. `memory` in the vault,
   * where nothing is written to this phone's disk — see `src/gallery/cache.ts`.
   */
  cache?: MediaCache;
  /**
   * What can be done to this one photograph. Every one of them closes the peek
   * on the way, because several open a sheet — and a sheet is drawn by the app,
   * not by this `Modal`, so one opened from in here would come up behind it.
   */
  actions?: Action[];
  onClose: () => void;
}) {
  const { width, height } = useWindowDimensions();
  const insets = useSafeAreaInsets();
  const open = useSharedValue(0);
  const playing = useSharedValue(0);

  // Held past the moment the parent lets go, so the photograph can shrink back
  // into the square it came from instead of vanishing out of a blur. The rows
  // are held with it and for the same reason: the grid computes them from the
  // open target, so letting them empty on the way out would resize the
  // photograph halfway through the animation putting it back.
  const [held, setHeld] = useState<PeekTarget | null>(null);
  const [rows, setRows] = useState<Action[]>([]);
  const [armed, setArmed] = useState<string | null>(null);
  const [broken, setBroken] = useState(false);

  useEffect(() => {
    if (!target) return;
    setHeld(target);
    setRows(actions);
    setArmed(null);
    setBroken(false);
    playing.value = 0;
    void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Medium);
    open.value = withTiming(1, { duration: OPEN_MS, easing: Easing.out(Easing.cubic) });
    // `actions` is read here and deliberately not depended on: the rows are
    // computed from `target`, so re-running on their identity would only reset
    // a row somebody has armed.
  }, [target, open, playing]);

  /**
   * Putting it back.
   *
   * Driven by the target going away rather than by whatever asked it to, which
   * is the only arrangement that works: a row that files a photograph and then
   * closes this is not calling the button underneath the photograph, it is
   * calling the grid — and while this watched only that button, every one of
   * those rows left the lifted photograph sitting on screen over the thing it
   * had just done.
   */
  useEffect(() => {
    if (target || !held) return;
    open.value = withTiming(0, { duration: CLOSE_MS, easing: Easing.in(Easing.cubic) }, (done) => {
      if (done) runOnJS(setHeld)(null);
    });
  }, [target, held, open]);

  const item = held?.item ?? null;

  // A square, because a square is what the grid holds: the stored thumbnail is
  // a centre crop, and the full frame is the viewer's business.
  //
  // Smaller when there are actions under it, and by enough that the last of
  // them is never the one off the bottom of a small screen. The photograph is
  // the thing you are already looking at; the rows are the thing you opened
  // this to reach.
  const side = Math.min(width - MARGIN * 2, height * (rows.length > 0 ? 0.42 : 0.55));
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
      onRequestClose={onClose}
    >
      <Animated.View style={[StyleSheet.absoluteFill, scrimStyle]} pointerEvents="none">
        <BlurView intensity={38} tint="dark" style={StyleSheet.absoluteFill} />
        <View style={styles.dim} />
      </Animated.View>

      <Pressable
        style={StyleSheet.absoluteFill}
        onPress={onClose}
        accessibilityRole="button"
        accessibilityLabel="Close"
      />

      <Animated.View style={[styles.card, cardStyle]} pointerEvents="none">
        <Image
          style={StyleSheet.absoluteFill}
          source={still}
          contentFit="cover"
          cachePolicy={cache}
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

      <Animated.View style={[styles.below, { top: to.y + side }, captionStyle]}>
        <Text variant="small" tone="muted" style={styles.caption}>
          {new Date(item.taken_at).toLocaleString(undefined, {
            weekday: 'short',
            day: 'numeric',
            month: 'long',
            year: 'numeric',
            hour: 'numeric',
            minute: '2-digit',
          })}
        </Text>

        {rows.length > 0 ? (
          <View style={styles.actions}>
            {rows.map((action) => {
              const live = armed === action.key;
              const tint = action.disabled
                ? color.faint
                : action.tone === 'destructive'
                  ? color.destructive
                  : color.foreground;
              return (
                <Pressable
                  key={action.key}
                  accessibilityRole="button"
                  accessibilityLabel={action.label}
                  accessibilityState={{ disabled: !!action.disabled }}
                  disabled={action.disabled}
                  onPress={() => {
                    // Two taps for the one thing here that cannot be taken
                    // back by tapping again, and the second is in the same
                    // place as the first — which is the whole argument for
                    // arming a row rather than raising an alert over a modal.
                    if (action.armed && !live) {
                      setArmed(action.key);
                      return;
                    }
                    setArmed(null);
                    action.onPress?.();
                  }}
                  style={({ pressed }) => [
                    styles.action,
                    live && styles.armed,
                    pressed && styles.actionPressed,
                  ]}
                >
                  <Feather name={live ? 'alert-triangle' : action.icon} size={17} color={tint} />
                  <Text variant="body" numberOfLines={1} style={[styles.actionLabel, { color: tint }]}>
                    {live ? `Tap again to ${action.label.toLowerCase()}` : action.label}
                  </Text>
                  {action.note && action.disabled ? (
                    <Text variant="caption" tone="faint">
                      {action.note}
                    </Text>
                  ) : null}
                </Pressable>
              );
            })}
          </View>
        ) : null}
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
  // Sized to its contents rather than stretched to the bottom edge, so a tap
  // in the space under the last row still dismisses the peek.
  below: {
    position: 'absolute',
    left: space.xl,
    right: space.xl,
    paddingTop: space.lg,
  },
  caption: { textAlign: 'center' },
  actions: { marginTop: space.lg, gap: space.sm },
  action: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    minHeight: 48,
    paddingHorizontal: space.md,
    borderRadius: radius.lg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: 'rgba(22,22,26,0.86)',
  },
  armed: { borderColor: color.destructive },
  actionLabel: { flex: 1 },
  actionPressed: { opacity: 0.7 },
});
