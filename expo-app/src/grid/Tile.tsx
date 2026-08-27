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
import { memo, useCallback, useMemo, useState } from 'react';
import { StyleSheet, Text as RNText, View } from 'react-native';
import Animated, { useAnimatedStyle, type SharedValue } from 'react-native-reanimated';

import { color, radius, space, text } from '../theme';
import { rectAt, type Places } from './geometry';

/**
 * One square of the grid.
 *
 * Laid out at `box` — the largest cell size the running transition will reach —
 * and scaled down to whatever the zoom currently wants, for the reason the
 * browser's Tile gives: resizing the box every frame means re-rasterising every
 * thumbnail on screen every frame, while scaling one that is already big enough
 * costs a composite. The scale runs on the UI thread from the shared zoom, so
 * React sees none of it.
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
}: {
  item: TimelineItem;
  box: number;
  thumb: ThumbSize;
  /** Where this tile sits at each zoom level. See grid/geometry. */
  places: Places;
  z: SharedValue<number>;
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

  const onError = useCallback(() => {
    if (size === undefined) return;
    setMissing((held) => (held.includes(size) ? held : [...held, size]));
  }, [size]);

  const style = useAnimatedStyle(() => {
    const r = rectAt(places, z.value);
    return {
      transform: [{ translateX: r.x }, { translateY: r.y }, { scale: r.size / box }],
    };
  });

  return (
    <Animated.View style={[styles.tile, { width: box, height: box }, style]}>
      <View style={styles.chrome}>
        {attempt && source ? (
          <Image
            style={styles.picture}
            source={source}
            contentFit="cover"
            // The bytes are the same whatever the tile is drawn at, so the disk
            // cache is what makes scrolling back through a year of photographs
            // free. WEB_TO_MOBILE § 3.6 is about the day table and the pages
            // for exactly this reason: the thumbnails already have a cache.
            cachePolicy="memory-disk"
            // Short enough to read as the picture arriving rather than as an
            // animation, and it covers the swap when a zoom changes the size.
            transition={120}
            // Keyed by the asset rather than the URL, so following a zoom to a
            // larger rendition replaces the picture in place instead of tearing
            // it down and leaving a hole where a photograph was.
            recyclingKey={item.id}
            onError={onError}
            accessibilityIgnoresInvertColors
          />
        ) : (
          <View style={styles.blank}>
            {size === undefined ? (
              <Feather name="alert-triangle" size={Math.min(22, box * 0.28)} color={color.faint} />
            ) : null}
          </View>
        )}

        {item.live && item.live !== 'failed' ? <LiveGlyph /> : null}

        {item.kind === 'video' && item.duration ? (
          <View style={styles.duration}>
            <PlayGlyph />
            <RNText style={styles.durationText}>{formatDuration(item.duration)}</RNText>
          </View>
        ) : null}
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
}: {
  box: number;
  places: Places;
  z: SharedValue<number>;
}) {
  const style = useAnimatedStyle(() => {
    const r = rectAt(places, z.value);
    return {
      transform: [{ translateX: r.x }, { translateY: r.y }, { scale: r.size / box }],
    };
  });

  return (
    <Animated.View style={[styles.tile, { width: box, height: box }, style]}>
      <View style={styles.chrome} />
    </Animated.View>
  );
});

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
  // The picture sits inside the square rather than being it, so that Phase 5's
  // selection can draw the tile back from its cell without fighting the
  // transform the zoom loop writes to the node above.
  chrome: { flex: 1, overflow: 'hidden', backgroundColor: color.tile },
  picture: { flex: 1 },
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
