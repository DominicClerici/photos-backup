import {
  formatDuration,
  media,
  thumbSizeFallbacks,
  thumbVariant,
  type ThumbSize,
  type TimelineItem,
} from '@photobackup/core';
import { Feather } from '@expo/vector-icons';
import { Image } from 'expo-image';
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { StyleSheet, Text as RNText, View } from 'react-native';
import Animated, {
  Easing,
  useAnimatedStyle,
  useSharedValue,
  withDelay,
  withTiming,
  type SharedValue,
} from 'react-native-reanimated';

import type { MediaCache } from '../gallery/cache';
import { color, radius, space, text } from '../theme';
import { rectAt, type Places } from './geometry';

/** How long a photograph takes to arrive, once there is one to draw. */
const ARRIVE_MS = 120;

/**
 * How far the picked tile's edges are clipped away, and over how long.
 *
 * A clip rather than an inset: the photograph must not move or resize when it
 * is chosen — a picture that jumps a pixel and re-rasterises on every tap is
 * the whole of what made this feel like a stutter — so what closes over it is a
 * border in the board's own colour. Nothing inside is laid out again.
 */
const PICKED = 3;
const PICK_MS = 50;

/**
 * The hold, before it becomes a lift.
 *
 * Nothing happens for `PRESS_LEAD_MS`, which is what keeps a tap and a scroll
 * from twitching the grid. After that the tile draws back, reaching
 * `PRESS_SCALE` exactly as the hold completes — so the shrink is the countdown,
 * and the lift begins from where the finger has already taken it.
 */
export const PRESS_LEAD_MS = 150;
export const PRESS_SCALE = 0.9;
/**
 * How long a tile has to be held before it lifts.
 *
 * Here rather than beside the gesture that enforces it, because the shrink
 * above is the visible half of the same number: the tile has to arrive at
 * `PRESS_SCALE` exactly as the hold completes, and two files disagreeing about
 * when that is would be a tile that finished shrinking and then waited.
 */
export const HOLD_MS = 380;
/** Letting go, whether the hold completed or not. Also the peek's way back. */
export const RELEASE_MS = 150;

/**
 * Everything about a square that moves: where the zoom puts it, how far the
 * hold has drawn it back, and how far the clip has closed over it.
 *
 * Three styles rather than one, and the split is the point. The transform runs
 * every frame of a zoom; the box is written only when the zoom crosses a level;
 * the clip only when somebody picks something. Reanimated flushes whichever of
 * them ran in a single batch of property updates, so the box and the scale that
 * divides by it can never be a frame apart — which is what a pinch used to
 * flash at every level it crossed.
 *
 * Both animations are guarded on the value having actually changed. A scroll
 * mounts a couple of hundred of these a second, and a square that started two
 * animations on the way in to say that nothing had happened would be paying
 * that cost across the whole grid.
 */
function useMotion({
  box,
  places,
  z,
  pressed,
  selected,
}: {
  box: SharedValue<number>;
  places: Places;
  z: SharedValue<number>;
  pressed: boolean;
  selected: boolean;
}) {
  const press = useSharedValue(pressed ? 1 : 0);
  const holding = useRef(pressed);
  useEffect(() => {
    if (holding.current === pressed) return;
    holding.current = pressed;
    press.value = pressed
      ? withDelay(
          PRESS_LEAD_MS,
          // The rest of the hold, so the tile arrives at PRESS_SCALE on the
          // same frame the peek decides to lift it.
          withTiming(1, { duration: HOLD_MS - PRESS_LEAD_MS, easing: Easing.out(Easing.quad) }),
        )
      : withTiming(0, { duration: RELEASE_MS, easing: Easing.out(Easing.quad) });
  }, [pressed, press]);

  const pick = useSharedValue(selected ? 1 : 0);
  const picked = useRef(selected);
  useEffect(() => {
    if (picked.current === selected) return;
    picked.current = selected;
    pick.value = withTiming(selected ? 1 : 0, { duration: PICK_MS, easing: Easing.linear });
  }, [selected, pick]);

  const boxStyle = useAnimatedStyle(() => ({ width: box.value, height: box.value }));

  const style = useAnimatedStyle(() => {
    const b = box.value;
    const r = rectAt(places, z.value);
    // The cell's own transform grows from the top left, because that is the
    // corner every rect the grid computes describes. The hold's does not: it is
    // the square drawing back into itself, so it is walked to the middle of the
    // box and out again. Exact at rest, where the two walks cancel and both
    // scales are one.
    const half = b / 2;
    return {
      transform: [
        { translateX: r.x },
        { translateY: r.y },
        { scale: r.size / b },
        { translateX: half },
        { translateY: half },
        { scale: 1 - (1 - PRESS_SCALE) * press.value },
        { translateX: -half },
        { translateY: -half },
      ],
    };
  });

  const clipStyle = useAnimatedStyle(() => ({ borderWidth: PICKED * pick.value }));

  return { boxStyle, style, clipStyle };
}

/**
 * One square of the grid.
 *
 * Laid out at `box` — the largest cell size the running transition will reach —
 * and scaled down to whatever the zoom currently wants, for the reason the
 * browser's Tile gives: resizing the box every frame means re-rasterising every
 * thumbnail on screen every frame, while scaling one that is already big enough
 * costs a composite.
 *
 * `box` is a shared value rather than a prop, and that is not a detail. React
 * commits a width on one schedule and Reanimated writes a transform on another,
 * so a box that changed with a render was a frame of every tile on screen drawn
 * at the new size with the old scale — the flash a pinch used to show at every
 * level it crossed. Read from the UI thread, the two land in the same batch of
 * property updates and there is nothing to see. See `Grid`'s `boxSize`.
 *
 * Memoized, and it matters more here than it would in a browser: a scroll
 * re-renders the grid whenever the mounted range moves, and the several hundred
 * tiles that did not change must not reconcile their images again.
 */
export const Tile = memo(function Tile({
  item,
  box,
  thumb,
  places,
  z,
  cache,
  selected = false,
  pressed = false,
}: {
  item: TimelineItem;
  /** The size the tile is laid out at, in points. Written by the zoom. */
  box: SharedValue<number>;
  thumb: ThumbSize;
  /** Where this tile sits at each zoom level. See grid/geometry. */
  places: Places;
  z: SharedValue<number>;
  /**
   * How long these bytes may be kept. `memory-disk` everywhere but the vault,
   * where a decrypted thumbnail written to disk would outlive the password that
   * decrypted it — see `src/gallery/cache.ts`.
   */
  cache: MediaCache;
  /** Whether this photograph is in the selection. */
  selected?: boolean;
  /** Whether a finger is being held on it. See the hold in `Grid`. */
  pressed?: boolean;
}) {
  const [missing, setMissing] = useState<ThumbSize[]>([]);

  // A tile that 404'd while its derivative job was still running must try again
  // once the poller says it landed. The component is keyed by id and survives
  // that transition, so without this it would stay broken forever.
  const [seen, setSeen] = useState(item.state);
  if (seen !== item.state) {
    setSeen(item.state);
    if (missing.length > 0) setMissing([]);
  }

  // "pending" means the thumbnail provably does not exist yet, so asking for it
  // would 404 by design. Every other state is worth an attempt: a metadata job
  // can fail *after* writing the thumbnail, and that photo should still appear.
  const attempt = item.state !== 'pending';

  // A size that was never rendered is a gap in what is stored rather than a
  // missing asset, so the next-best file is a better answer than nothing.
  // Larger before smaller, because downscaling costs nothing and upscaling
  // shows. Undefined once even the base rendition has failed.
  const size = useMemo(
    () => thumbSizeFallbacks(thumb).find((option) => !missing.includes(option)),
    [thumb, missing],
  );

  const source = useMemo(
    () => (size === undefined ? null : media(item.id, thumbVariant(size))),
    [item.id, size],
  );

  /**
   * The rendition actually on screen, or undefined until the first has landed.
   *
   * A zoom changes which file a tile should be drawing, and the swap used to be
   * a dissolve: two half-transparent copies of the same photograph, with the
   * dark tile showing through both of them for sixty milliseconds. Across a
   * screenful at once that is the flicker a pinch was full of.
   *
   * So the rendition already drawing keeps drawing, and the one being swapped
   * to is mounted *underneath* it and left to load out of sight. When it lands,
   * the layer on top is dropped and the layer behind is already showing the
   * same photograph, at the same rect, under the same contentFit. There is
   * nothing to fade and no frame in which the square is empty.
   *
   * Which is why the layers are a keyed array rather than two slots in the
   * markup, and that is the whole of the fix. React reconciles an array by key,
   * so the layer holding a picture keeps its view whichever position it ends up
   * in. Two fixed slots reconcile by position instead — and under that, the
   * moment the wanted size changed, the loaded image was unmounted and a fresh
   * one mounted in its place while a second fresh one mounted behind it. Two
   * blank views and no picture between them, on every tile on screen, at every
   * level a pinch crossed. That was the flash.
   */
  const [painted, setPainted] = useState<ThumbSize | undefined>(undefined);

  /**
   * The rendition currently being asked for, as `onLoad` needs to read it.
   *
   * A load can land for a file the zoom has already moved past. Promoting that
   * one would name a layer the next render does not draw, and the render after
   * that would have to mount it again from nothing — one blank view laid over
   * the photograph, which is the single thing this arrangement exists to stop.
   */
  const want = useRef(size);
  useEffect(() => {
    want.current = size;
  }, [size]);

  const arrived = useCallback((landed: ThumbSize) => {
    if (want.current === landed) setPainted(landed);
  }, []);

  const onError = useCallback(() => {
    if (size === undefined) return;
    setMissing((held) => (held.includes(size) ? held : [...held, size]));
  }, [size]);

  const held = useMemo(
    () =>
      painted !== undefined && size !== undefined && painted !== size
        ? media(item.id, thumbVariant(painted))
        : null,
    [item.id, painted, size],
  );

  /**
   * What to draw, in the order the layers were added and never any other.
   *
   * At most two: the one holding the picture, and the one arriving behind it.
   * Insertion order means the holder is always first, so promoting the arrival
   * is a deletion from in front of it rather than a reordering — React never
   * moves a view here either, only ever adds one at the end or drops the head.
   */
  const layers: { size: ThumbSize; source: ReturnType<typeof media>; holding: boolean }[] = [];
  if (held !== null && painted !== undefined) {
    layers.push({ size: painted, source: held, holding: true });
  }
  if (size !== undefined && source !== null) {
    layers.push({ size, source, holding: false });
  }

  const { boxStyle, style, clipStyle } = useMotion({ box, places, z, pressed, selected });

  return (
    <Animated.View style={[styles.tile, boxStyle, style]}>
      <View style={styles.chrome}>
        {attempt && layers.length > 0 ? (
          layers.map((layer) => (
            <Image
              // Keyed by the rendition and reconciled as an array, so the layer
              // that is already drawing keeps its view while the other one is
              // added behind it and later taken from in front of it. See
              // `layers` above; this key is the whole of why nothing flashes.
              key={layer.size}
              // The holder sits over the arrival, so a rendition being swapped
              // to is invisible for the whole of the time it takes to load.
              style={layer.holding ? overStyle : styles.picture}
              source={layer.source}
              contentFit="cover"
              // The bytes are the same whatever the tile is drawn at, so the
              // disk cache is what makes scrolling back through a year of
              // photographs free. WEB_TO_MOBILE § 3.6 is about the day table
              // and the pages for exactly this reason: the thumbnails already
              // have a cache.
              //
              // Except inside the vault, which is the one grid that keeps
              // nothing.
              cachePolicy={cache}
              // Short enough to read as the picture arriving rather than as an
              // animation — and only ever for the very first one. A swap has
              // the photograph already over it and fades through nothing.
              transition={painted === undefined ? ARRIVE_MS : 0}
              recyclingKey={item.id}
              onLoad={layer.holding ? undefined : () => arrived(layer.size)}
              onError={layer.holding ? undefined : onError}
              accessibilityIgnoresInvertColors
            />
          ))
        ) : (
          <View style={styles.blank}>
            {size === undefined ? (
              <Feather name="alert-triangle" size={20} color={color.faint} />
            ) : null}
          </View>
        )}

        {/* What closes over a picked photograph, in the board's own colour, so
            a wall of chosen pictures reads as a grid of separate things rather
            than one dimmed sheet. A border rather than an inset because the
            photograph underneath must not move by so much as a pixel. */}
        <Animated.View style={[StyleSheet.absoluteFill, styles.clip, clipStyle]} pointerEvents="none" />

        {item.live && item.live !== 'failed' ? <LiveGlyph /> : null}

        {item.kind === 'video' && item.duration ? (
          <View style={styles.duration}>
            <PlayGlyph />
            <RNText style={styles.durationText}>{formatDuration(item.duration)}</RNText>
          </View>
        ) : null}

        {/* Top right, because the bottom right is a video's length and the top
            left is the Live mark. Only ever on a chosen photograph: the tick is
            the mark of being picked, and a grid of empty rings over everything
            is a grid nobody can see the photographs in. */}
        {selected ? <Tick /> : null}
      </View>
    </Animated.View>
  );
});

/**
 * A square the grid knows the place and size of but not yet the picture for.
 *
 * The grid draws these before it has asked for a single photograph, because the
 * day table fixes every tile's position up front — which is the whole point of
 * them. Scrolling never runs out of grid, the scroll extent never changes under
 * the thumb as pages land, and a photo that arrives replaces the exact square
 * that was standing in for it.
 *
 * Deliberately inert, and a shade darker than the square a Tile shows for a
 * derivative that is still being generated: a screenful at the smallest zoom is
 * a couple of thousand of these, and animating all of them would cost more than
 * fetching the photographs they are waiting for. The two stay distinguishable —
 * this one is waiting on the network, that one on the worker.
 */
export const Skeleton = memo(function Skeleton({
  box,
  places,
  z,
  selected = false,
  pressed = false,
}: {
  box: SharedValue<number>;
  places: Places;
  z: SharedValue<number>;
  /**
   * A square with no photograph in it yet is still a position, and a drag that
   * crosses it selects it — the selection is runs of indices precisely so that
   * it can cover ground nobody has downloaded. So it draws the tick too.
   */
  selected?: boolean;
  pressed?: boolean;
}) {
  const { boxStyle, style, clipStyle } = useMotion({ box, places, z, pressed, selected });

  return (
    <Animated.View style={[styles.tile, boxStyle, style]}>
      <View style={styles.chrome}>
        <Animated.View style={[StyleSheet.absoluteFill, styles.clip, clipStyle]} pointerEvents="none" />
        {selected ? <Tick /> : null}
      </View>
    </Animated.View>
  );
});

function Tick() {
  return (
    <View style={styles.tick}>
      <Feather name="check" size={12} color={color.primaryForeground} />
    </View>
  );
}

/** Apple's concentric rings, the mark everyone already reads as "Live". */
function LiveGlyph() {
  return (
    <View style={styles.live}>
      <View style={styles.liveOuter} />
      <View style={styles.liveInner} />
    </View>
  );
}

function PlayGlyph() {
  return <View style={styles.play} />;
}

const styles = StyleSheet.create({
  tile: {
    position: 'absolute',
    top: 0,
    left: 0,
    // Every rect the grid computes is the top-left corner of a cell, so the
    // scale that shrinks a box down to it has to grow from the same corner.
    transformOrigin: 'top left',
  },
  // The picture sits inside the square rather than being it, so that the clip
  // that marks a selection can close over it without fighting the transform the
  // zoom loop writes to the node above.
  chrome: { flex: 1, overflow: 'hidden', backgroundColor: color.tile },
  clip: { borderColor: color.background },
  // Absolute rather than flexed, because there are two of them during a swap
  // and a column of two flexed children is two half-height photographs.
  picture: { position: 'absolute', top: 0, left: 0, right: 0, bottom: 0 },
  // What puts the rendition already drawn over the one still loading. A
  // z-order rather than a different position in the markup, because the two
  // layers must keep their views across the swap and only their stacking may
  // change — see `layers` in Tile.
  over: { zIndex: 2 },
  // A derivative that has not been generated yet, and one that cannot be drawn
  // at all. Both are squares rather than spinners: a screenful at the smallest
  // zoom is a couple of thousand of them, and the difference that matters is
  // between this and the flat `tile` a Skeleton shows — that one is waiting on
  // the network, this one on the worker.
  blank: {
    flex: 1,
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: color.tileSheen,
  },

  tick: {
    position: 'absolute',
    // Clear of the clip, so the mark keeps its margin from the edge somebody
    // can actually see rather than from the one the border has covered.
    top: PICKED + 4,
    right: PICKED + 4,
    width: 18,
    height: 18,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.pill,
    borderWidth: 1.4,
    borderColor: color.primary,
    backgroundColor: color.primary,
  },

  live: { position: 'absolute', top: 5, left: 5, width: 14, height: 14 },
  liveOuter: {
    position: 'absolute',
    inset: 0,
    borderRadius: radius.pill,
    borderWidth: 1.4,
    borderColor: 'rgba(255,255,255,0.9)',
  },
  liveInner: {
    position: 'absolute',
    top: 4.6,
    left: 4.6,
    width: 4.8,
    height: 4.8,
    borderRadius: radius.pill,
    backgroundColor: 'rgba(255,255,255,0.9)',
  },

  duration: {
    position: 'absolute',
    right: space.xs + 1,
    bottom: space.xs,
    flexDirection: 'row',
    alignItems: 'center',
    gap: 3,
  },
  durationText: {
    ...text.caption,
    color: '#ffffff',
    fontVariant: ['tabular-nums'],
    textShadowColor: 'rgba(0,0,0,0.7)',
    textShadowOffset: { width: 0, height: 1 },
    textShadowRadius: 3,
  },
  play: {
    width: 0,
    height: 0,
    borderTopWidth: 4,
    borderBottomWidth: 4,
    borderLeftWidth: 7,
    borderTopColor: 'transparent',
    borderBottomColor: 'transparent',
    borderLeftColor: '#ffffff',
  },
});

/** The holding layer's style, composed once: a swap must allocate nothing. */
const overStyle = [styles.picture, styles.over];
