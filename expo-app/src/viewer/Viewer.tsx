import {
  fetchAnalysis,
  fetchAsset,
  type AssetAnalysis,
  type AssetDetail,
  type TimelineItem,
} from '@photobackup/core';
import type { TimelineState } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { router } from 'expo-router';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Pressable, StyleSheet, useWindowDimensions, View } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  Easing,
  runOnJS,
  useAnimatedReaction,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
} from 'react-native-reanimated';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { mediaCacheFor } from '../gallery/cache';
import { color, radius, space } from '../theme';
import { Sheet, Text } from '../ui';
import { Page, type Stage } from './Page';
import { Panel } from './Panel';
import { saveOriginal } from './save';

/** How far a photograph can be zoomed, and where a double tap lands on the way. */
const MAX_SCALE = 6;
const DOUBLE_SCALE = 2.5;

/** Past this, the preview is being drawn larger than it is, and the original is fetched. */
const SHARPEN_AT = 1.4;

/** How long the pager takes to settle onto a page after a swipe. */
const PAGE_MS = 240;

/** How far, or how fast, a downward drag has to go before it closes the viewer. */
const DISMISS_DISTANCE = 110;
const DISMISS_VELOCITY = 900;

/** How much of a flick's speed counts towards which page it was aimed at. */
const FLICK = 0.15;

/** How far a drag has to go before it has decided whether it is a swipe or a dismiss. */
const AXIS_SLOP = 8;

/** Long enough that a finger settling before a swipe never trips it. */
const HOLD_MS = 320;

/** The bar's controls, between shadcn's icon and icon-lg — the browser's 34px. */
const CONTROL = 34;

/**
 * One photograph, full screen, and the timeline either side of it.
 *
 * A port of the browser's `Viewer` in everything it shows and a reimplementation
 * of everything it does. `useViewer` is deliberately not shared: it encodes the
 * open asset in the query string and pushes a history entry so that Back closes
 * the viewer, and here expo-router's stack does exactly that job — the route is
 * the history entry. See WEB_TO_MOBILE § 4.
 *
 * The pager is three pages wide and absolutely placed. `scrollX` is where the
 * timeline has been scrolled to, in points, and every page draws itself at
 * `index * width - scrollX`. That number lives on the UI thread, so changing
 * photograph is not a render and a swipe cannot be interrupted by a page
 * landing behind it; React's only job is to decide which three positions are
 * mounted. It is the same division of labour the grid makes, for the same
 * reason.
 */
export function Viewer({
  timeline,
  at,
  sealed = false,
}: {
  timeline: TimelineState;
  at: number;
  /**
   * Whether this timeline is inside the vault.
   *
   * One thing follows from it and it is not cosmetic: the renditions are kept
   * in memory rather than written to disk, so a decrypted preview does not
   * outlive the fifteen minutes the password bought. The panel below already
   * declines to look anything up for a sealed asset, for its own reasons.
   */
  sealed?: boolean;
}) {
  const { width, height } = useWindowDimensions();
  const insets = useSafeAreaInsets();
  const { total, at: itemAt, request } = timeline;

  const [index, setIndex] = useState(() => clampJS(at, 0, Math.max(0, total - 1)));
  const [chrome, setChrome] = useState(true);
  const [pressed, setPressed] = useState(false);
  const [panelOpen, setPanelOpen] = useState(false);
  // Kept across navigation rather than reset per photograph: Snapchat memories
  // arrive in runs, and someone who turned the captions off to look at one
  // almost always wants the next one the same way.
  const [overlayOn, setOverlayOn] = useState(true);
  const [full, setFull] = useState(false);
  const [detail, setDetail] = useState<AssetDetail | null>(null);
  const [analysis, setAnalysis] = useState<AssetAnalysis | null>(null);

  /**
   * The photograph at each mounted position, and whatever has been fetched for
   * it.
   *
   * `timeline` is a dependency because `itemAt` reads through a ref — the store
   * holds one slot per photograph in the archive and mutates it in place, and
   * its own identity is the only thing React can see change when a page lands.
   * Which makes this the seam the React Compiler needs, for the reason
   * `grid/Grid.tsx` gives at greater length: given a stable `itemAt` and an
   * unchanged `index`, it would otherwise be entitled to hold a spinner on
   * screen while the photograph it is waiting for arrived behind it.
   */
  const pages = useMemo(() => {
    const out: { n: number; item: TimelineItem | undefined }[] = [];
    for (let n = index - 1; n <= index + 1; n++) {
      if (n >= 0 && n <= Math.max(0, total - 1)) out.push({ n, item: itemAt(n) });
    }
    return out;
  }, [index, total, itemAt, timeline]);

  const cache = mediaCacheFor(sealed);
  const item = pages.find((page) => page.n === index)?.item;
  const hasOverlay = detail?.has_overlay ?? false;
  // A video whose playback rendition is ready owns the horizontal drag: its
  // scrubber is one, and a pager listening for the same gesture would take it.
  // So on those pages the swipe is off and the bar grows two chevrons — see the
  // note on `VideoStage` in Page.tsx.
  const scrubbing = item?.kind === 'video' && item.playback_state === 'ready';
  // Only a photograph zooms. A video is played rather than examined, and a
  // pinch that scaled the page a `VideoView` sits in would be enlarging the
  // player along with the picture.
  const zoomable = item?.kind === 'image';

  const scrollX = useSharedValue(index * width);
  const scale = useSharedValue(1);
  const tx = useSharedValue(0);
  const ty = useSharedValue(0);
  const dismiss = useSharedValue(0);
  /** The photograph's drawn size, which is what panning is bounded by. */
  const fitW = useSharedValue(width);
  const fitH = useSharedValue(height);
  const stage: Stage = useMemo(
    () => ({ scrollX, scale, tx, ty, dismiss }),
    [scrollX, scale, tx, ty, dismiss],
  );

  const startX = useSharedValue(0);
  const startTx = useSharedValue(0);
  const startTy = useSharedValue(0);
  const startScale = useSharedValue(1);
  /** 0 while a drag has not decided, 1 across, 2 down. */
  const axis = useSharedValue(0);
  const from = useSharedValue(0);

  const last = Math.max(0, total - 1);

  /** Everything about one photograph, forgotten when the pager lands on another. */
  const settle = useCallback(
    (to: number) => {
      setIndex(to);
      setFull(false);
      scale.value = 1;
      tx.value = 0;
      ty.value = 0;
      fitW.value = width;
      fitH.value = height;
    },
    [scale, tx, ty, fitW, fitH, width, height],
  );

  const onFit = useCallback(
    (w: number, h: number) => {
      const s = Math.min(width / w, height / h);
      fitW.value = w * s;
      fitH.value = h * s;
    },
    [fitW, fitH, width, height],
  );

  // A rotation, or a phone that changed its mind about how wide it is: the page
  // has moved, and the pager has to be told where it went. Harmlessly true the
  // rest of the time — a settled pager is already at exactly this offset.
  useEffect(() => {
    scrollX.value = index * width;
  }, [scrollX, index, width]);

  useEffect(() => {
    if (total > 0 && index > last) settle(last);
  }, [total, index, last, settle]);

  // What the viewer is looking at, and the two either side, which is what makes
  // a swipe land on a photograph rather than on a spinner. Retired on the way
  // out — a viewer that has closed should not still be pinning a page.
  useEffect(() => {
    request('viewer', index - 1, index + 2);
  }, [request, index]);
  useEffect(() => () => request('viewer', 0, 0), [request]);

  useEffect(() => {
    if (!item) return;
    setDetail(null);
    const controller = new AbortController();
    fetchAsset(item.id, controller.signal)
      .then(setDetail)
      .catch(() => {
        // The picture still shows; only the panel goes empty.
      });
    return () => controller.abort();
  }, [item?.id]);

  // What the models said, fetched only while the panel is open and asked again
  // for each photograph stepped to with it open. Separate from the detail
  // because the two are wanted at different moments: a photograph of a terminal
  // carries kilobytes of recognised text that nobody with the panel shut has
  // asked to download.
  useEffect(() => {
    if (!item || !panelOpen) return;
    setAnalysis(null);
    const controller = new AbortController();
    fetchAnalysis(item.id, controller.signal)
      .then(setAnalysis)
      .catch(() => {
        // The rest of the panel still draws, and this half goes on saying it is
        // reading — a photograph is not the place to report that the server is
        // unreachable.
      });
    return () => controller.abort();
  }, [item?.id, panelOpen]);

  // Latched rather than followed: once the original is here, zooming back out
  // and in again should not fetch it a second time. Dropped by `settle`, which
  // is the only place the photograph changes.
  useAnimatedReaction(
    () => scale.value > SHARPEN_AT,
    (sharp) => {
      if (sharp) runOnJS(setFull)(true);
    },
  );

  const leave = useCallback(() => {
    // The route is the history entry — closing the viewer is popping it, which
    // is also what the system Back gesture does, which is the whole reason this
    // is a route.
    router.back();
  }, []);

  const toggleChrome = useCallback(() => setChrome((on) => !on), []);
  const hold = useCallback((on: boolean) => setPressed(on), []);

  /** The chevrons a video gets in place of the swipe it cannot have. */
  const step = useCallback(
    (by: number) => {
      const to = clampJS(index + by, 0, last);
      if (to === index) return;
      scrollX.value = to * width;
      settle(to);
    },
    [index, last, scrollX, settle, width],
  );

  // ── The gestures ───────────────────────────────────────────────────────────

  const gesture = useMemo(() => {
    const limitX = (k: number) => {
      'worklet';
      return Math.max(0, (fitW.value * k - width) / 2);
    };
    const limitY = (k: number) => {
      'worklet';
      return Math.max(0, (fitH.value * k - height) / 2);
    };
    const page = (v: number) => {
      'worklet';
      return clamp(v, 0, last * width);
    };

    const pinch = Gesture.Pinch()
      .enabled(zoomable)
      .onStart(() => {
        startScale.value = scale.value;
        startTx.value = tx.value;
        startTy.value = ty.value;
      })
      .onUpdate((e) => {
        const next = clamp(startScale.value * e.scale, 1, MAX_SCALE);
        // The point under the fingers stays under them: a photograph drawn at
        // `k·p + t` keeps the content point at the focus still when
        // `t₁ = f − (k₁/k₀)(f − t₀)`.
        const k = next / startScale.value;
        const fx = e.focalX - width / 2;
        const fy = e.focalY - height / 2;
        scale.value = next;
        tx.value = clamp(fx - k * (fx - startTx.value), -limitX(next), limitX(next));
        ty.value = clamp(fy - k * (fy - startTy.value), -limitY(next), limitY(next));
      })
      .onEnd(() => {
        if (scale.value > 1.01) return;
        scale.value = withTiming(1, { duration: 160 });
        tx.value = withTiming(0, { duration: 160 });
        ty.value = withTiming(0, { duration: 160 });
      });

    const pan = Gesture.Pan()
      .enabled(!scrubbing)
      .onStart(() => {
        startX.value = scrollX.value;
        startTx.value = tx.value;
        startTy.value = ty.value;
        from.value = Math.round(scrollX.value / width);
        axis.value = 0;
      })
      .onUpdate((e) => {
        if (scale.value > 1.01) {
          // Panning a zoomed photograph, and — once it has run out of picture
          // to give — handing what is left over to the pager. Which is what
          // makes a zoomed photograph feel like a page rather than a trap: keep
          // dragging past its edge and the next one comes.
          const lx = limitX(scale.value);
          const want = startTx.value + e.translationX;
          const held = clamp(want, -lx, lx);
          tx.value = held;
          ty.value = clamp(
            startTy.value + e.translationY,
            -limitY(scale.value),
            limitY(scale.value),
          );
          scrollX.value = page(startX.value - (want - held));
          return;
        }

        if (axis.value === 0) {
          const dx = Math.abs(e.translationX);
          const dy = Math.abs(e.translationY);
          if (dx < AXIS_SLOP && dy < AXIS_SLOP) return;
          axis.value = dx > dy ? 1 : 2;
        }
        if (axis.value === 1) scrollX.value = page(startX.value - e.translationX);
        else dismiss.value = e.translationY;
      })
      .onEnd((e) => {
        if (axis.value === 2) {
          if (
            e.translationY > DISMISS_DISTANCE ||
            e.velocityY > DISMISS_VELOCITY
          ) {
            runOnJS(leave)();
            return;
          }
          dismiss.value = withTiming(0, { duration: 180, easing: Easing.out(Easing.cubic) });
          return;
        }

        // Where the flick was aimed, held to one page either side: a swipe is a
        // step, not a scroll, however fast it was.
        const aimed = (scrollX.value - e.velocityX * FLICK) / width;
        const to = clamp(
          Math.round(aimed),
          Math.max(0, from.value - 1),
          Math.min(last, from.value + 1),
        );
        const moved = to !== from.value;
        scrollX.value = withTiming(
          to * width,
          { duration: PAGE_MS, easing: Easing.out(Easing.cubic) },
          (done) => {
            // Only when the pager actually changed photograph. A zoomed
            // photograph panned back from its own edge lands here too, and
            // resetting its zoom would undo the thing somebody was doing.
            if (done && moved) runOnJS(settle)(to);
          },
        );
      });

    const doubleTap = Gesture.Tap()
      .enabled(zoomable)
      .numberOfTaps(2)
      .maxDelay(220)
      .onEnd((e) => {
        if (scale.value > 1.01) {
          scale.value = withTiming(1, { duration: 200 });
          tx.value = withTiming(0, { duration: 200 });
          ty.value = withTiming(0, { duration: 200 });
          return;
        }
        const fx = e.x - width / 2;
        const fy = e.y - height / 2;
        const k = DOUBLE_SCALE;
        scale.value = withTiming(k, { duration: 200 });
        tx.value = withTiming(clamp(-fx * (k - 1), -limitX(k), limitX(k)), { duration: 200 });
        ty.value = withTiming(clamp(-fy * (k - 1), -limitY(k), limitY(k)), { duration: 200 });
      });

    // The delay is the double tap's window: a single tap cannot be a single tap
    // until the second one has failed to arrive. 220ms is the shortest that
    // still catches a deliberate double, and it is the whole cost of having
    // both gestures on the same photograph.
    const singleTap = Gesture.Tap()
      .numberOfTaps(1)
      .onEnd(() => runOnJS(toggleChrome)());

    const press = Gesture.LongPress()
      // Only where there is something under the photograph to reveal. Anywhere
      // else this would be a finger resting before a swipe, and a gesture that
      // won that race would be a swipe that did not happen.
      .enabled(!scrubbing && (item?.live === 'ready' || hasOverlay))
      .minDuration(HOLD_MS)
      .maxDistance(14)
      .onStart(() => runOnJS(hold)(true))
      .onFinalize(() => runOnJS(hold)(false));

    return Gesture.Simultaneous(
      pinch,
      Gesture.Race(press, Gesture.Exclusive(doubleTap, singleTap), pan),
    );
  }, [
    axis,
    dismiss,
    fitH,
    fitW,
    from,
    hasOverlay,
    height,
    hold,
    item?.live,
    last,
    leave,
    scale,
    scrollX,
    scrubbing,
    settle,
    zoomable,
    startScale,
    startTx,
    startTy,
    startX,
    toggleChrome,
    tx,
    ty,
    width,
  ]);

  // ── Drawing ────────────────────────────────────────────────────────────────

  const shownChrome = useSharedValue(1);
  useEffect(() => {
    shownChrome.value = withTiming(chrome ? 1 : 0, { duration: 160 });
  }, [chrome, shownChrome]);

  const backdrop = useAnimatedStyle(() => ({
    opacity: 1 - Math.min(Math.abs(dismiss.value) / (height * 0.6), 0.85),
  }));
  const barStyle = useAnimatedStyle(() => ({ opacity: shownChrome.value }));

  return (
    <View style={styles.root}>
      <Animated.View style={[StyleSheet.absoluteFill, styles.backdrop, backdrop]} />

      <GestureDetector gesture={gesture}>
        <View style={styles.stage} collapsable={false}>
          {pages.map(({ n, item: shown }) => (
            <Page
              key={n}
              item={shown}
              detail={n === index ? detail : null}
              left={n * width}
              active={n === index}
              stage={stage}
              width={width}
              height={height}
              overlayOn={overlayOn}
              pressed={pressed && n === index}
              chrome={chrome}
              full={full && n === index}
              cache={cache}
              onFit={onFit}
            />
          ))}
        </View>
      </GestureDetector>

      <Animated.View
        style={[styles.bar, { paddingTop: insets.top + space.xs }, barStyle]}
        pointerEvents={chrome ? 'box-none' : 'none'}
      >
        <Control icon="x" label="Close the viewer" onPress={leave} />

        <Text variant="small" tone="muted" numberOfLines={1} style={styles.count}>
          {`${(index + 1).toLocaleString()} of ${total.toLocaleString()}`}
        </Text>

        {scrubbing ? (
          <>
            <Control
              icon="chevron-left"
              label="Previous"
              disabled={index === 0}
              onPress={() => step(-1)}
            />
            <Control
              icon="chevron-right"
              label="Next"
              disabled={index === last}
              onPress={() => step(1)}
            />
          </>
        ) : null}

        {hasOverlay ? (
          <Control
            icon="layers"
            label={overlayOn ? 'Hide the overlay' : 'Show the overlay'}
            on={!overlayOn}
            onPress={() => setOverlayOn((on) => !on)}
          />
        ) : null}

        <Control
          icon="info"
          label="Details"
          on={panelOpen}
          onPress={() => setPanelOpen((open) => !open)}
        />
        <Control
          icon="download"
          label="Save to your photos"
          disabled={!detail}
          onPress={() => {
            if (detail) void saveOriginal(detail.id, detail.filename);
          }}
        />
      </Animated.View>

      <Sheet open={panelOpen} onClose={() => setPanelOpen(false)} title={detail?.filename}>
        <Panel detail={detail} analysis={analysis} />
      </Sheet>
    </View>
  );
}

function Control({
  icon,
  label,
  onPress,
  on = false,
  disabled = false,
}: {
  icon: React.ComponentProps<typeof Feather>['name'];
  label: string;
  onPress: () => void;
  /** Drawn as held down, the way the browser's `aria-pressed` buttons are. */
  on?: boolean;
  disabled?: boolean;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityState={{ selected: on, disabled }}
      onPress={onPress}
      disabled={disabled}
      hitSlop={6}
      style={({ pressed }) => [
        styles.control,
        on && styles.controlOn,
        pressed && !disabled && styles.controlPressed,
      ]}
    >
      <Feather
        name={icon}
        size={19}
        color={disabled ? color.faint : on ? color.foreground : color.mutedForeground}
      />
    </Pressable>
  );
}

function clamp(value: number, low: number, high: number): number {
  'worklet';
  return value < low ? low : value > high ? high : value;
}

/** The same, off the UI thread, where the worklet directive would be a lie. */
function clampJS(value: number, low: number, high: number): number {
  return value < low ? low : value > high ? high : value;
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  backdrop: { backgroundColor: color.viewer },
  stage: { flex: 1, overflow: 'hidden' },
  bar: {
    position: 'absolute',
    top: 0,
    left: 0,
    right: 0,
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.xs,
    paddingHorizontal: space.sm,
    paddingBottom: space.sm,
  },
  count: { flex: 1, marginLeft: space.xs, fontVariant: ['tabular-nums'] },
  control: {
    width: CONTROL,
    height: CONTROL,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.md,
  },
  controlOn: { backgroundColor: color.muted },
  controlPressed: { opacity: 0.6 },
});
