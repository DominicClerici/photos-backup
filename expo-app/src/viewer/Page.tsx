import {
  media,
  thumbVariant,
  type AssetDetail,
  type TimelineItem,
} from '@photobackup/core';
import { Feather } from '@expo/vector-icons';
import { Image, type ImageLoadEventData } from 'expo-image';
import { useVideoPlayer, VideoView, type VideoSource } from 'expo-video';
import { useEffect, useMemo, useRef, useState } from 'react';
import { ActivityIndicator, StyleSheet, View } from 'react-native';
import Animated, {
  useAnimatedStyle,
  useSharedValue,
  withTiming,
  type SharedValue,
} from 'react-native-reanimated';

import type { MediaCache } from '../gallery/cache';
import { color, radius, space } from '../theme';
import { Button, Text } from '../ui';
import { saveOriginal } from './save';

/** The dissolve either side of a Live Photo, and of the photograph under a caption. */
const FADE_MS = 120;

/** How far the photograph shrinks at the end of a full dismiss drag. */
const DISMISS_SHRINK = 0.25;

/**
 * The zoom and the dismiss, as the UI thread sees them.
 *
 * One set for the whole pager rather than one per page, because only one page
 * is ever the active one and only the active page can be zoomed. A page that is
 * not active draws itself square on, at whatever `scrollX` says, and that is the
 * whole of its animation.
 */
export interface Stage {
  /** Where the pager has scrolled to, in points across the whole timeline. */
  scrollX: SharedValue<number>;
  scale: SharedValue<number>;
  /** The zoomed photograph's offset, in screen points, after the scale. */
  tx: SharedValue<number>;
  ty: SharedValue<number>;
  /** How far a downward drag has carried the photograph towards leaving. */
  dismiss: SharedValue<number>;
}

/**
 * One photograph, at one position in the timeline.
 *
 * Placed by `left - scrollX`, which is a number the UI thread owns: the page's
 * home is a fixed multiple of the screen width and the pager is a single
 * offset, so changing photograph never moves anything on the JS thread and a
 * swipe cannot be interrupted by a page landing. What React decides is only
 * *which* three of these are mounted.
 */
export function Page({
  item,
  detail,
  left,
  active,
  stage,
  width,
  height,
  overlayOn,
  pressed,
  chrome,
  full,
  cache,
  onFit,
}: {
  /** Undefined while the page holding this position is still being fetched. */
  item: TimelineItem | undefined;
  /** Only ever the active page's: it is the only one whose extras are drawn. */
  detail: AssetDetail | null;
  left: number;
  active: boolean;
  stage: Stage;
  width: number;
  height: number;
  /** Whether a Snapchat memory shows its caption layer. Kept across navigation. */
  overlayOn: boolean;
  /** A finger is being held on the photograph — see `PhotoStage`. */
  pressed: boolean;
  /** Whether the bar and the hints are showing. */
  chrome: boolean;
  /** Somebody has zoomed past what the preview can answer; fetch the original. */
  full: boolean;
  /**
   * How long these bytes may be kept.
   *
   * `memory` for a photograph out of the vault, and it is the whole of what
   * this app does differently there: a decrypted preview written to
   * `expo-image`'s disk cache would still be sitting in the sandbox, in the
   * clear, after the server had re-locked — which is the one thing the vault
   * exists to prevent. See src/vault.
   */
  cache: MediaCache;
  onFit: (width: number, height: number) => void;
}) {
  const { scrollX, scale, tx, ty, dismiss } = stage;

  const style = useAnimatedStyle(() => {
    const base = left - scrollX.value;
    if (!active) return { transform: [{ translateX: base }] };
    const away = Math.min(Math.abs(dismiss.value) / height, 1);
    return {
      transform: [
        { translateX: base + tx.value },
        { translateY: ty.value + dismiss.value },
        { scale: scale.value * (1 - away * DISMISS_SHRINK) },
      ],
    };
  });

  return (
    <Animated.View style={[styles.page, { width, height }, style]} pointerEvents="box-none">
      {item === undefined ? (
        <ActivityIndicator color={color.mutedForeground} />
      ) : item.kind === 'video' ? (
        <VideoStage
          item={item}
          active={active}
          plain={Boolean(detail?.has_overlay) && !overlayOn}
          controls={chrome}
          filename={detail?.filename ?? ''}
          cache={cache}
        />
      ) : (
        <PhotoStage
          item={item}
          active={active}
          hasOverlay={Boolean(detail?.has_overlay)}
          overlayOn={overlayOn}
          pressed={pressed}
          chrome={chrome}
          full={full}
          cache={cache}
          onFit={onFit}
        />
      )}
    </Animated.View>
  );
}

/**
 * The photograph, and whatever is behind it — the three seconds a Live Photo
 * carries, or the picture under a Snapchat memory's caption layer.
 *
 * One gesture reveals both, because they are the same gesture: press and hold,
 * let go and it goes back. Which one a photo has is a property of the photo and
 * never of the press, and nothing in this archive has ever had both. That is
 * the browser's design and its words; what is different here is only that the
 * hold is a `LongPress` rather than a captured pointer.
 *
 * Three layers, and the order is the argument:
 *
 * - the preview, rendered from the blob, and the first thing drawn. The grid's
 *   thumbnail used to be laid under it for the frame before it arrived, and it
 *   cannot be: a thumbnail is a square centre crop and a photograph is not
 *   square, so what it actually did was sit behind every landscape shot with
 *   its own edges showing above and below the picture. A moment of nothing is
 *   a better answer than a moment of the wrong shape;
 * - the original, but only once somebody has zoomed past what the preview can
 *   answer — WEB_TO_MOBILE § 4 asks for "preview then original", and *then* is
 *   the whole of it: a phone should not carry fifty megapixels across a network
 *   to draw them at four hundred points wide;
 * - and whatever the hold reveals, on top.
 */
function PhotoStage({
  item,
  active,
  hasOverlay,
  overlayOn,
  pressed,
  chrome,
  full,
  cache,
  onFit,
}: {
  item: TimelineItem;
  active: boolean;
  hasOverlay: boolean;
  overlayOn: boolean;
  pressed: boolean;
  chrome: boolean;
  full: boolean;
  cache: MediaCache;
  onFit: (width: number, height: number) => void;
}) {
  const live = active && item.live === 'ready';
  // The caption layer is off either because the toggle says so or because a
  // finger is on the photograph, and the two mean the same thing to the picture.
  const plain = hasOverlay && (!overlayOn || pressed);

  const preview = useMemo(() => media(item.id, 'preview'), [item.id]);
  const bare = useMemo(
    () => (hasOverlay ? media(item.id, 'preview/plain') : null),
    [item.id, hasOverlay],
  );
  // Never while the caption layer is off: the original is the composite, so
  // fetching it there would put back the very thing the toggle took away.
  const original = useMemo(
    () => (full && !plain ? media(item.id, 'original') : null),
    [item.id, full, plain],
  );

  const bareShown = useSharedValue(0);
  const bareStyle = useAnimatedStyle(() => ({ opacity: bareShown.value }));
  useEffect(() => {
    bareShown.value = withTiming(plain ? 1 : 0, { duration: FADE_MS });
  }, [plain, bareShown]);

  // The natural size of whatever has been decoded, kept so that a page which
  // loaded its preview while it was a neighbour can report its shape the moment
  // it becomes the active one. `onLoad` fires once per image, and by then the
  // page somebody is looking at may not have been this one — without this, the
  // pan on a photograph swiped to would be bounded by the screen rather than by
  // the picture, and a portrait shot would slide sideways into the black.
  const natural = useRef<{ w: number; h: number } | null>(null);
  useEffect(() => {
    const seen = natural.current;
    if (active && seen) onFit(seen.w, seen.h);
  }, [active, onFit]);

  const [sharp, setSharp] = useState(false);
  const sharpShown = useSharedValue(0);
  const sharpStyle = useAnimatedStyle(() => ({ opacity: sharpShown.value }));
  useEffect(() => {
    if (!original) setSharp(false);
  }, [original]);
  useEffect(() => {
    sharpShown.value = withTiming(sharp ? 1 : 0, { duration: FADE_MS });
  }, [sharp, sharpShown]);

  return (
    <>
      <Image
        style={StyleSheet.absoluteFill}
        source={preview}
        contentFit="contain"
        cachePolicy={cache}
        transition={FADE_MS}
        // What the pan limits are measured against. A photograph drawn
        // `contain` occupies less than the screen in one axis, and panning it
        // into the empty half would be the picture sliding out from under the
        // finger that is holding it.
        onLoad={(e: ImageLoadEventData) => {
          natural.current = { w: e.source.width, h: e.source.height };
          if (active) onFit(e.source.width, e.source.height);
        }}
        accessibilityIgnoresInvertColors
      />

      {bare ? (
        // Mounted from the start, which is what makes the hold instant and what
        // leaves the composite showing for the moment before these bytes
        // arrive.
        <Animated.View style={[StyleSheet.absoluteFill, bareStyle]} pointerEvents="none">
          <Image
            style={StyleSheet.absoluteFill}
            source={bare}
            contentFit="contain"
            cachePolicy={cache}
            transition={0}
          />
        </Animated.View>
      ) : null}

      {original ? (
        <Animated.View style={[StyleSheet.absoluteFill, sharpStyle]} pointerEvents="none">
          <Image
            style={StyleSheet.absoluteFill}
            source={original}
            contentFit="contain"
            cachePolicy={cache}
            transition={0}
            onLoad={() => setSharp(true)}
            // A rendition the archive holds and this phone cannot decode is a
            // preview left showing, which is the right answer and not an error
            // worth a notice over a photograph.
            onError={() => setSharp(false)}
          />
        </Animated.View>
      ) : null}

      {live ? <LiveLayer id={item.id} playing={pressed} /> : null}

      {chrome && !pressed && (live || (hasOverlay && overlayOn)) ? (
        <View style={styles.hint} pointerEvents="none">
          <Feather name={live ? 'aperture' : 'layers'} size={12} color={color.mutedForeground} />
          <Text variant="caption" tone="muted">
            {live ? 'Hold to play' : 'Hold to hide'}
          </Text>
        </View>
      ) : null}
    </>
  );
}

/**
 * The three seconds a Live Photo carries, over the still of the same moment.
 *
 * The still stays lit underneath rather than crossing over with this, for the
 * browser's reason: two pictures dissolving into each other are both
 * part-transparent halfway through, and the backdrop showing between them dims
 * the photograph just as the motion starts.
 *
 * Muted, as it is in `grid/Peek.tsx`, and for the reason given there — this is
 * motion attached to a photograph and it has never carried sound anybody
 * wanted. The browser tries for sound and falls back; a phone that started
 * playing audio over somebody's music because they rested a finger on a picture
 * would be making a different decision, not the same one.
 */
function LiveLayer({ id, playing }: { id: string; playing: boolean }) {
  const source = useMemo<VideoSource>(() => media(id, 'live/preview'), [id]);
  const player = useVideoPlayer(source, (p) => {
    p.muted = true;
    p.loop = false;
  });

  const shown = useSharedValue(0);
  const style = useAnimatedStyle(() => ({ opacity: shown.value }));
  const settle = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(settle.current), []);

  useEffect(() => {
    clearTimeout(settle.current);
    if (playing) {
      player.currentTime = 0;
      player.play();
      return;
    }
    shown.value = withTiming(0, { duration: FADE_MS });
    // Pausing and rewinding wait for the fade: doing either to a clip that is
    // still half on screen is the jump cut the fade is there to hide.
    settle.current = setTimeout(() => {
      player.pause();
      player.currentTime = 0;
    }, FADE_MS);
  }, [playing, player, shown]);

  return (
    <Animated.View style={[StyleSheet.absoluteFill, style]} pointerEvents="none">
      <VideoView
        style={StyleSheet.absoluteFill}
        player={player}
        contentFit="contain"
        nativeControls={false}
        onFirstFrameRender={() => {
          shown.value = withTiming(1, { duration: FADE_MS });
        }}
      />
    </Animated.View>
  );
}

/**
 * A video, and for a Snapchat memory the choice between the two renditions of
 * it.
 *
 * There is no press-and-hold here and there cannot be: the caption is in the
 * pixels, because nothing will composite a PNG over a playing video, so
 * revealing the photograph underneath means fetching a different file. The
 * toggle swaps the source and the playhead is carried across, which is as close
 * to the still's gesture as a second download gets.
 *
 * The controls come and go with the rest of the chrome, and that is a decision
 * about gestures rather than about tidiness: a native scrubber is a horizontal
 * drag inside a pager that is also listening for one, and the two cannot both
 * have it. So while the controls are up the pager lets go — see `Viewer` — and
 * a tap puts them away and gives the swipe back.
 */
function VideoStage({
  item,
  active,
  plain,
  controls,
  filename,
  cache,
}: {
  item: TimelineItem;
  active: boolean;
  plain: boolean;
  controls: boolean;
  filename: string;
  cache: MediaCache;
}) {
  if (item.playback_state !== 'ready') {
    return <Unplayable item={item} filename={filename} />;
  }
  // Only the page somebody is looking at gets a player. The two either side are
  // a still each, which is what makes swiping through a run of clips cost one
  // decoder rather than three.
  if (!active) {
    return (
      <Image
        style={StyleSheet.absoluteFill}
        source={media(item.id, thumbVariant())}
        contentFit="contain"
        cachePolicy={cache}
        transition={0}
      />
    );
  }
  return <Playing item={item} plain={plain} controls={controls} />;
}

function Playing({
  item,
  plain,
  controls,
}: {
  item: TimelineItem;
  plain: boolean;
  controls: boolean;
}) {
  const [broken, setBroken] = useState(false);
  // A rendition the transcode has not caught up with yet is a 404, and the
  // composite is a better answer than a black rectangle.
  const bare = plain && !broken;
  const source = useMemo<VideoSource>(
    () => media(item.id, bare ? 'playback/plain' : 'playback'),
    [item.id, bare],
  );

  const player = useVideoPlayer(source, (p) => {
    p.loop = false;
    p.timeUpdateEventInterval = 0.5;
    p.play();
  });

  // Sampled as it plays rather than read at the moment of the swap: by the time
  // the source has changed, the player has already been reset by it and the
  // playhead is gone.
  const at = useRef(0);
  const restore = useRef(0);

  useEffect(() => {
    const ticking = player.addListener('timeUpdate', (e) => {
      at.current = e.currentTime;
    });
    const watching = player.addListener('statusChange', (e) => {
      if (e.status !== 'readyToPlay') {
        // Only the bare rendition is worth falling back from: it is the one the
        // transcode may not have caught up with. An error on the composite is
        // the video itself, and swapping to the same source again would say
        // nothing new.
        if (e.status === 'error' && bare) setBroken(true);
        return;
      }
      if (restore.current > 0) {
        player.currentTime = restore.current;
        restore.current = 0;
      }
      player.play();
    });
    return () => {
      ticking.remove();
      watching.remove();
    };
  }, [player, bare]);

  // The swap, not the first load: opening a video should start it at the
  // beginning, and only a toggle between the two renditions has a place to
  // return to.
  const swapped = useRef(false);
  useEffect(() => {
    if (swapped.current) restore.current = at.current;
    swapped.current = true;
  }, [source]);

  return (
    <VideoView
      style={StyleSheet.absoluteFill}
      player={player}
      contentFit="contain"
      nativeControls={controls}
      allowsPictureInPicture={false}
    />
  );
}

/**
 * A video with nothing to play — either the transcode has not run or it could
 * not. Both say the same reassuring thing, because it is true: the original is
 * stored untouched and this is a rendition, not the archive.
 */
function Unplayable({ item, filename }: { item: TimelineItem; filename: string }) {
  const failed = item.playback_state === 'failed';
  return (
    <View style={styles.unplayable}>
      <Feather name={failed ? 'alert-triangle' : 'clock'} size={26} color={color.faint} />
      <Text variant="small" tone="muted" style={styles.centred}>
        {failed
          ? 'This video could not be converted for playback.'
          : 'Preparing a version this phone can play…'}
      </Text>
      <Text variant="caption" tone="faint" style={styles.centred}>
        The original is stored untouched and can always be saved.
      </Text>
      <Button
        label="Save the original"
        icon="download"
        onPress={() => {
          void saveOriginal(item.id, filename || item.id);
        }}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  page: {
    position: 'absolute',
    top: 0,
    left: 0,
    alignItems: 'center',
    justifyContent: 'center',
  },
  hint: {
    position: 'absolute',
    top: space.md,
    left: space.md,
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.xs + 2,
    borderRadius: radius.pill,
    backgroundColor: 'rgba(22,22,26,0.72)',
    paddingHorizontal: space.md,
    paddingVertical: 5,
  },
  unplayable: {
    alignItems: 'center',
    gap: space.md,
    paddingHorizontal: space.xl,
  },
  centred: { textAlign: 'center' },
});
