import {
  dayAt,
  dayIndexOf,
  describeAction,
  fetchStates,
  frameAt,
  headless,
  itemAtPoint,
  layoutLevel,
  MAX_ZOOM,
  metricsFor,
  nounFor,
  thumbSizeFor,
  visibleItems,
  ZOOM_LEVELS,
  type Day,
  type ItemRange,
  type LevelLayout,
  type TimelineFilter,
} from '@photobackup/core';
import {
  useSelection,
  useSelectionScope,
  useViewScope,
  type SelectionActions,
  type TimelineState,
} from '@photobackup/core/react';
import { BlurView } from 'expo-blur';
import { router, useIsFocused } from 'expo-router';
import * as Haptics from 'expo-haptics';
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { StyleSheet, useWindowDimensions, View } from 'react-native';
import { Gesture, GestureDetector } from 'react-native-gesture-handler';
import Animated, {
  Easing,
  runOnJS,
  scrollTo,
  useAnimatedReaction,
  useAnimatedRef,
  useAnimatedScrollHandler,
  useAnimatedStyle,
  useSharedValue,
  withTiming,
  type SharedValue,
} from 'react-native-reanimated';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { askToFile } from '../actions';
import { mediaCacheFor } from '../gallery/cache';
import { color, radius, space } from '../theme';
import { Button, TAB_BAR_CLEARANCE, Text, type Action } from '../ui';
import { DatePill } from './DatePill';
import {
  cellsOf,
  clamp,
  headerHeightsOf,
  heightsOf,
  placesFor,
  rectAt,
  topsFor,
  valueAt,
  zoomForCell,
} from './geometry';
import { Peek, type PeekTarget } from './Peek';
import { Scrubber } from './Scrubber';
import { HOLD_MS, PRESS_SCALE, Skeleton, Tile } from './Tile';
import { rememberLevel, savedLevel } from './zoomStore';

/** How often unfinished tiles ask whether their derivative landed. */
const POLL_MS = 4000;

/**
 * Fetched above and below the viewport, as a fraction of its height.
 *
 * Wider than what is drawn, because a placeholder that is already on screen has
 * missed its chance to be a photograph. A flick spends about a second crossing
 * this much grid, which is roughly what a page costs.
 */
const FETCH_OVERSCAN = 2.5;

/** Mounted above and below the viewport, as a fraction of its height. */
const OVERSCAN = 0.75;

/**
 * The mounted range is rounded outwards to a multiple of this.
 *
 * Which is the phone's answer to a problem the browser does not have. There,
 * every scroll recomputes the visible range and re-renders, and the DOM keeps
 * up. Here a render walks React Native's shadow tree and crosses to the native
 * side, so a grid that re-rendered per scroll event would drop the scroll it
 * was trying to follow. Quantizing means the mounted set changes once every few
 * rows instead — a handful of renders per screenful, with the overscan above
 * covering the ground in between.
 */
const CHUNK = 48;

/** How far the zoom must travel before React is told. About a tenth of a level. */
const ZOOM_STEP = 0.1;

/** One step of zoom, matching lib/zoom's ZOOM_MS so both apps settle alike. */
const SETTLE_MS = 300;

/**
 * How deep the bands at the top and bottom of the grid are, and how fast a
 * finger held at the very edge of one drags the timeline past itself.
 *
 * A drag-selection has to be able to cover more than a screenful, and the only
 * hand doing the dragging is the one that would otherwise be scrolling. So the
 * edges pull, in proportion to how far into them the finger has gone — which
 * is what makes a slow crawl through the band controllable and a shove into
 * the corner fast.
 */
const EDGE_BAND = 110;
const EDGE_SPEED = 22;

/** The margin the grid keeps at each side, and what the day headings hang on. */
const GUTTER = space.md;

/**
 * The space between two tiles.
 *
 * A hairline rather than the browser's four points. A phone's grid is read at
 * arm's length and a photograph is the only thing on it worth any pixels, so
 * the gap is there to say that two pictures are two pictures and nothing more.
 */
const TILE_GAP = 1;

/** Room above the first heading, under the status bar and the floating date. */
const TOP_ROOM = space.xxl;

/**
 * The point a zoom happens around: a tile, how far down it the fingers are, and
 * where on the screen that is to stay.
 *
 * A tile rather than a scroll offset, because an offset means something
 * different at every level — the whole timeline is nearly three times taller at
 * the smallest cell size than at the largest. `places` is the same twenty-one
 * numbers a tile carries, which is all the UI thread needs to place it at any
 * continuous zoom.
 */
interface Anchor {
  places: number[];
  /** How far down the anchored tile the held point is, 0–1. */
  frac: number;
  /** Where the held point should stay, measured from the top of the screen. */
  screenY: number;
}

/** What the JS side needs to answer a scroll or a zoom without waiting for a render. */
interface Env {
  levels: LevelLayout[];
  days: Day[];
  total: number;
  height: number;
  /** The whole timeline's height at each level, for clamping an auto-scroll. */
  heights: number[];
  padTop: number;
  padBottom: number;
}

/**
 * The timeline, drawn.
 *
 * Everything about *where* things go comes from `@photobackup/core`'s
 * `layout.ts`, unmodified and with its tests intact: the day model, the seven
 * zoom levels, the blend between two of them, and the binary searches that turn
 * a scroll offset into a range of items. What is written here is the part that
 * puts a `<View>` where the maths says, plus the two things a phone needs that
 * a browser does not — the zoom runs on the UI thread (see grid/geometry), and
 * the mounted set changes in chunks rather than per frame.
 *
 * The scroll extent is `totalHeight`, exact from the first frame and before a
 * single photograph has been fetched. That is the property the day table exists
 * to give, and it is what makes the scrubber trustworthy: two thirds of the way
 * down the rail is two thirds of the way down the archive, not two thirds of
 * the way down whatever happened to load.
 */
export function Grid({
  timeline,
  filter,
  actions,
  sortable = true,
  immersive = true,
  empty,
}: {
  timeline: TimelineState;
  /**
   * Which collection this grid is of, or undefined for the library. Published
   * to the sort-and-filter control, which uses it to decide what there is left
   * to filter by — inside the Videos category every item is a video, and inside
   * an album nothing is in no album. See core's `facetsFor`.
   */
  filter?: TimelineFilter;
  /**
   * What can be done to a selection made here. Published for as long as this
   * grid is mounted, which is what makes the floating control appear on the
   * screens that have a gallery and nowhere else, and what drops a selection
   * when one is navigated away from.
   */
  actions: SelectionActions;
  /**
   * Whether this grid claims the sort and the filters. The search results do
   * not: the order there is the ranking and the filter is the query, so a
   * control offering "Newest" over a relevance ranking would be one that either
   * does nothing or throws the answer away.
   */
  sortable?: boolean;
  /**
   * Whether this grid owns the top of the screen.
   *
   * The library tab does: photographs run under the clock and the battery, the
   * way a photo app's do, which is what the blurred strip at the top is for. A
   * collection and the search results have a header above them instead, so
   * there is nothing passing behind anything and the room under the status bar
   * has already been taken.
   */
  immersive?: boolean;
  /** What to say when the timeline is empty. */
  empty?: string;
}) {
  const insets = useSafeAreaInsets();
  const { width: screenWidth } = useWindowDimensions();
  // Everything in the vault is drawn from memory and never written down. See
  // `src/gallery/cache.ts`, which owns both halves of that rule.
  const cache = mediaCacheFor(filter?.kind === 'vault');
  const { days, total, ready, loading, stale, error, retry, at: itemAt, request, patch } = timeline;

  const [viewport, setViewport] = useState(0);
  const inner = Math.max(1, screenWidth - GUTTER * 2);

  // The board starts under the status bar and stops above the floating tab bar,
  // which is drawn over the content rather than beside it. These two are the
  // only conversion between what `layout.ts` says and where things are on
  // screen: board y = scroll offset − padTop, and board x = screen x − GUTTER.
  const padTop = (immersive ? insets.top : 0) + TOP_ROOM;
  const padBottom = insets.bottom + TAB_BAR_CLEARANCE + space.lg;

  // A timeline with no days reserves no room for the headings it will not draw,
  // which turns the grid into the flat wall of tiles an order by length
  // actually is. Everything else is unchanged by it: one day, one heading of no
  // height, every tile hanging under it.
  const flat = headless(days);
  const levels = useMemo(
    () =>
      ZOOM_LEVELS.map((cap) =>
        layoutLevel(
          days,
          metricsFor(inner, cap, { gap: TILE_GAP, ...(flat ? { headerHeight: 0 } : {}) }),
        ),
      ),
    [days, inner, flat],
  );

  const heights = useMemo(() => heightsOf(levels), [levels]);
  const cells = useMemo(() => cellsOf(levels), [levels]);
  const headerHeights = useMemo(() => headerHeightsOf(levels), [levels]);

  const scroller = useAnimatedRef<Animated.ScrollView>();
  /** The continuous zoom. One value, read by React and by every worklet here. */
  const z = useSharedValue(savedLevel());
  const scrollY = useSharedValue(0);
  /**
   * Whether a zoom owns the scroll offset.
   *
   * True from the moment a pinch activates until the settle after it finishes,
   * and read by the scroll handler, which stands down for the duration: while
   * this is up, the reaction that holds the anchor is the only thing writing
   * the offset and the only thing reporting it. See both.
   */
  const zooming = useSharedValue(false);

  /**
   * The size every tile is laid out at, in points.
   *
   * The cell of the level a running transition is heading *towards*, so a tile
   * is never laid out smaller than it is being drawn — which is the whole of
   * why the box is not simply the cell size. It is a shared value rather than
   * state for a reason worth stating: React commits a width on one schedule and
   * Reanimated writes a transform on another, so while this was a prop, every
   * level a pinch crossed gave a frame of the whole screen laid out at the new
   * box and still scaled for the old one. That is the flash the zoom had. Kept
   * on the UI thread, the width and the scale that divides by it are written in
   * the same batch, and a pinch crosses seven levels without a seam.
   */
  const boxSize = useSharedValue(cells[clamp(Math.round(z.value), 0, MAX_ZOOM)] ?? 1);

  const env = useRef<Env>({
    levels,
    days,
    total,
    height: 0,
    heights: [],
    padTop: 0,
    padBottom: 0,
  });
  /** Where the board's top edge has scrolled to, in board coordinates. */
  const top = useRef(0);
  const zoom = useRef(z.value);
  /**
   * Whether the grid is still moving under its own momentum.
   *
   * A tap that lands during a fling is somebody stopping the grid, not somebody
   * choosing a photograph — the scroll view takes it as a brake and would
   * otherwise also have opened whatever happened to be under the finger at the
   * moment it stopped.
   */
  const coasting = useRef(false);

  // Refreshed after every render and before the screen paints, so a scroll
  // event arriving between renders never measures against a day table that has
  // been replaced. The browser's Timeline keeps its `env` the same way.
  useLayoutEffect(() => {
    env.current = { levels, days, total, height: viewport, heights, padTop, padBottom };
  });

  /**
   * The settled zoom, and the rendition drawn into the tiles.
   *
   * The rendition follows the *largest* level a running transition will reach,
   * so zooming in asks for the sharper file as soon as the tiles are laid out
   * for it, and zooming out keeps the larger one until the grid has settled at
   * the smaller cell. Between gestures it is simply the settled level.
   *
   * Only the file changes here. Where the tiles are and how big they are drawn
   * is `boxSize` above, on the UI thread, and it moves several times a second
   * during a pinch — which is exactly why these two were separated: this one is
   * a render, and a render per frame of a zoom is a zoom that stutters.
   */
  const [level, setLevel] = useState(() => Math.round(z.value));
  const [sharpest, setSharpest] = useState(level);
  const thumb = thumbSizeFor(sharpest);

  const [window, setWindow] = useState<ItemRange>({ start: 0, end: 0 });
  const [wanted, setWanted] = useState<ItemRange>({ start: 0, end: 0 });
  const [pinching, setPinching] = useState(false);
  const [peek, setPeek] = useState<PeekTarget | null>(null);
  const [pinned, setPinned] = useState({ label: '', visible: false });

  // ── The selection ──────────────────────────────────────────────────────────
  //
  // The state machine is core's, unchanged and shared with the browser: runs of
  // indices, a drag that is always the run between where it began and where the
  // finger is *now* — which is the whole of how dragging back the way you came
  // undoes what you just picked, without anything having to remember the tiles
  // the gesture touched on its way out. What is written here is the part that
  // turns a point on the screen into an index.
  const { active: picking, selected, enter, toggle, beginDrag, moveDrag, endDrag } =
    useSelection();

  /**
   * Whether this is the grid being looked at.
   *
   * The browser needs no such question: a page navigated away from is
   * unmounted. A phone keeps every tab it has visited mounted behind the one on
   * top, so without this the library's grid would go on claiming the selection
   * from underneath an album, and the floating controls would appear over the
   * collections list — which has no grid in it at all.
   *
   * Blurred is not the same as gone: the viewer is a route above this one, so
   * opening a photograph blurs the grid it came out of. What that costs is a
   * selection, and a selection cannot be open at the time — a tap in selection
   * mode picks rather than opens.
   */
  const focused = useIsFocused();
  useSelectionScope(actions, focused);

  // Read from inside gesture callbacks, which are created once per gesture and
  // would otherwise close over whatever `picking` was at the time. Written in a
  // layout effect rather than during render, for the reason `env` is: a gesture
  // cannot begin before the frame it is answering has been committed, and a ref
  // written during render is the one shape the React Compiler — on for this app
  // and off for the browser — is entitled to be unhappy about.
  const pickingNow = useRef(picking);
  useLayoutEffect(() => {
    pickingNow.current = picking;
  });

  /**
   * Where a tile sits at every level, computed once and kept.
   *
   * Not a convenience: `Tile` is memoized, and a fresh array of twenty-one
   * numbers on every render would make every mounted tile re-render and every
   * one of their worklets re-serialize. The cache is dropped whenever the
   * geometry changes, which is the only time the answers do.
   */
  const places = useMemo(() => {
    const cache = new Map<number, number[]>();
    return (index: number): number[] => {
      const held = cache.get(index);
      if (held) return held;
      // Bounded well above anything a screenful plus overscan can mount, and
      // dropped whole rather than evicted one at a time: the entries are all
      // equally cheap to rebuild.
      if (cache.size > 4000) cache.clear();
      const day = dayIndexOf(days, index);
      const built = placesFor(levels, day, index - days[day].start);
      cache.set(index, built);
      return built;
    };
  }, [levels, days]);

  const tops = useMemo(() => {
    const cache = new Map<number, number[]>();
    return (day: number): number[] => {
      const held = cache.get(day);
      if (held) return held;
      const built = topsFor(levels, day);
      cache.set(day, built);
      return built;
    };
  }, [levels]);

  /**
   * Puts a position at the top of the viewport — what jumping to a date is.
   *
   * The day's heading rather than the tile itself, so a jump lands on "March
   * 2019" rather than a third of the way into it with the label off screen.
   */
  const jump = useCallback(
    (index: number) => {
      const e = env.current;
      if (e.total === 0 || e.days.length === 0 || e.height === 0) return;
      const day = dayIndexOf(e.days, clamp(index, 0, e.total - 1));
      const limit = Math.max(0, valueAt(e.heights, zoom.current) + e.padTop + e.padBottom - e.height);
      const to = clamp(valueAt(tops(day), zoom.current) + e.padTop - TOP_ROOM, 0, limit);
      scroller.current?.scrollTo({ y: to, animated: false });
      top.current = to - e.padTop;
    },
    [tops, scroller],
  );

  // What this grid is, said to the floating sort-and-filter control. Null from
  // a grid that does not claim it, which is the search results — see the
  // `sortable` prop.
  useViewScope(
    useMemo(
      () => (sortable && focused ? { filter, days, loading, jump } : null),
      [sortable, focused, filter, days, loading, jump],
    ),
  );

  /** What the grid is looking at, in item indices, at the zoom it is looking at it. */
  const rangeFor = useCallback((overscan: number): ItemRange => {
    const e = env.current;
    if (e.total === 0 || e.days.length === 0 || e.height === 0) return { start: 0, end: 0 };
    const frame = frameAt(e.levels, zoom.current);
    return visibleItems(e.days, e.total, frame, top.current, e.height, e.height * overscan);
  }, []);

  /**
   * Recomputes what is mounted and what is asked for.
   *
   * `widen` is what a running pinch passes: the far end of the scale holds
   * seven times as many tiles per screen as the near end, and a tile unmounted
   * halfway through a transition is a hole that opens while the grid is moving.
   * So during a gesture the set only grows, and it is cut back to what is
   * actually visible once the zoom has settled.
   */
  const settleRange = useCallback(
    (widen: boolean) => {
      const need = rangeFor(OVERSCAN);
      const start = Math.floor(need.start / CHUNK) * CHUNK;
      const end = Math.ceil(need.end / CHUNK) * CHUNK;

      setWindow((held) => {
        const next = widen
          ? { start: Math.min(held.start, start), end: Math.max(held.end, end) }
          : { start, end };
        return next.start === held.start && next.end === held.end ? held : next;
      });

      const fetch = rangeFor(FETCH_OVERSCAN);
      setWanted((held) => (held.start === fetch.start && held.end === fetch.end ? held : fetch));
    },
    [rangeFor],
  );

  /** The floating date: the heading that has scrolled off the top. */
  const settlePill = useCallback(() => {
    const e = env.current;
    const frame = frameAt(e.levels, zoom.current);
    const day = dayAt(e.days, frame, top.current);
    // Shown only once its own heading has gone, so it never sits directly above
    // a heading saying the same thing. A day with no label is a timeline with
    // no days, and there is no date to float.
    const visible = day != null && day.label !== '' && top.current > day.top + frame.headerHeight;
    const label = day?.label ?? '';
    setPinned((held) =>
      held.visible === visible && held.label === label ? held : { label, visible },
    );
  }, []);

  const onScrolled = useCallback(
    (y: number) => {
      top.current = y - padTop;
      settleRange(false);
      settlePill();
    },
    [padTop, settleRange, settlePill],
  );

  const onZoomed = useCallback(
    (v: number, y: number) => {
      zoom.current = v;
      // The offset arrives with the zoom because the zoom is what wrote it: the
      // scroll handler is standing down, so this is the only thing keeping
      // `top` current, and everything below measures against it.
      top.current = y - padTop;
      // Only ever upwards during a gesture: a tile drawing the file it is
      // already holding while the grid moves is a tile that is slightly soft
      // for a moment, and one that fetched a smaller file on the way past
      // would be a screenful of downloads thrown away at the other end.
      setSharpest((held) => Math.max(held, clamp(Math.ceil(v), 0, MAX_ZOOM)));
      settleRange(true);
      settlePill();
    },
    [padTop, settleRange, settlePill],
  );

  const onSettled = useCallback(
    (to: number, y: number) => {
      zoom.current = to;
      top.current = y - padTop;
      setLevel(to);
      setSharpest(to);
      setPinching(false);
      settleRange(false);
      settlePill();
      rememberLevel(to);
    },
    [padTop, settleRange, settlePill],
  );

  const scrollHandler = useAnimatedScrollHandler({
    onScroll: (e) => {
      scrollY.value = e.contentOffset.y;
      // A running zoom moves this scroll view itself, a frame at a time, and
      // reports where it put it — see the reaction below. Every one of those
      // frames comes back through here as a scroll event, and answering them
      // as well meant recomputing the mounted range, the fetch range and the
      // floating date on the JS thread sixty times a second, on top of the ten
      // a transition already costs. Worse, those recomputes ran `settleRange`
      // without `widen`, so they spent the gesture narrowing the very set the
      // zoom was widening — tiles unmounted mid-transition and mounted again a
      // frame later. That was most of the stutter: the thread that has to
      // mount what a zoom is asking for was busy measuring where it would go.
      if (zooming.value) return;
      runOnJS(onScrolled)(e.contentOffset.y);
    },
  });

  // ── The pinch ──────────────────────────────────────────────────────────────
  //
  // The gesture reports a scale; the cells the fingers came down on had a size;
  // the product is the size the fingers are now asking for, and `zoomForCell`
  // says where on the scale that is. Which makes the grid track the hand rather
  // than drift behind it, and makes letting go and starting again pick up
  // exactly where it left off. See grid/geometry.

  /** The point held still while the grid re-flows underneath it. */
  const anchor = useSharedValue<Anchor | null>(null);
  /**
   * The anchor that has been computed but not yet taken up.
   *
   * Two values rather than one, because the two moments are not the same. The
   * point under the fingers can be worked out as soon as they land; it may only
   * take effect once the gesture is actually driving the zoom. Between those, a
   * settle from the pinch before this one may still be running — and that
   * settle is held by the anchor already in `anchor`, which it must go on being
   * held by until the new gesture takes over. Writing the new one straight into
   * `anchor` moved the grid under an animation nobody was touching, which is
   * the jump a second pinch inside three hundred milliseconds used to make.
   */
  const armed = useSharedValue<Anchor | null>(null);
  const startCell = useSharedValue(0);
  const told = useSharedValue(z.value);
  /**
   * Whether the anchor is still crossing to the JS thread and back.
   *
   * The anchor needs the day model, which is why it cannot be computed on the
   * UI thread, which is why there is a round trip at all — and it is the one
   * round trip a pinch cannot afford to ignore, because until it lands there is
   * nothing to zoom around. See the gesture below.
   */
  const arming = useSharedValue(false);
  /** Whether this gesture has taken the zoom over yet. See `onUpdate`. */
  const driving = useSharedValue(false);
  /** The gesture's scale on the frame it did. */
  const baseScale = useSharedValue(1);

  /**
   * Pins the tile under the fingers, so the zoom happens around it.
   *
   * A tile rather than a scroll offset, because an offset means something
   * different at every level: the whole timeline is nearly three times taller
   * at the smallest cell size than at the largest. Computed here because it
   * needs the day model, and handed over as twenty-one numbers because that is
   * all the UI thread needs from then on.
   */
  const armAnchor = useCallback(
    (focalX: number, focalY: number) => {
      const e = env.current;
      if (e.total === 0 || e.days.length === 0) {
        armed.value = null;
        arming.value = false;
        return;
      }
      const at = frameAt(e.levels, zoom.current);
      const y = top.current + focalY;
      const index = itemAtPoint(e.days, e.total, at, focalX - GUTTER, y);
      const held = places(index);
      const rect = rectAt(held, zoom.current);
      // `screenY` here is provisional and is never the one used: the gesture
      // pins it again, exactly, on the frame it takes the zoom over. What this
      // round trip is for is the tile and the fraction down it, which are the
      // parts that need the day model.
      armed.value = {
        places: held,
        frac: clamp((y - rect.y) / (rect.size || 1), 0, 1),
        screenY: focalY,
      };
      arming.value = false;
    },
    [armed, arming, places],
  );

  /**
   * The scroll view stands down, now that the pinch is certainly a pinch.
   *
   * Separated from `armAnchor` above because that one now runs on the second
   * finger landing, and a two-finger drag that never becomes a pinch is a
   * scroll somebody is in the middle of. Only activation may stop it.
   */
  const beginPinch = useCallback(() => {
    setPinching(true);
  }, []);

  const pinch = useMemo(
    () =>
      Gesture.Pinch()
        /**
         * Anchored on the second finger landing, not on the gesture activating.
         *
         * The anchor needs the day model and so has to be computed on the JS
         * thread, and that round trip used to be made at activation — with `z`
         * already free to move. So the opening frames of every pinch had
         * nothing to zoom around: the grid re-flowed about the top of the
         * board while the scroll offset stayed where it was, and the frame the
         * anchor finally landed on snapped the photograph back under the
         * fingers. On a busy JS thread — which is precisely the thread a pinch
         * has just given a screenful of tiles to reconsider — that is several
         * frames, and it is the lurch a zoom opened with.
         *
         * Made here, the trip has the fifty-odd milliseconds RNGH spends
         * deciding that two fingers are a pinch, and it is almost always back
         * before the first `onUpdate`. When it is not, `onUpdate` waits.
         */
        .onTouchesDown((e) => {
          if (e.numberOfTouches !== 2) return;
          let x = 0;
          let y = 0;
          for (const touch of e.allTouches) {
            x += touch.x;
            y += touch.y;
          }
          arming.value = true;
          runOnJS(armAnchor)(x / e.allTouches.length, y / e.allTouches.length);
        })
        .onStart((e) => {
          zooming.value = true;
          driving.value = false;
          if (!arming.value && armed.value === null) {
            // Nothing landed as a pair — a third finger, or a pinch RNGH
            // recognised some other way. The old path, and still correct: ask
            // now, and let `onUpdate` wait for the answer.
            arming.value = true;
            runOnJS(armAnchor)(e.focalX, e.focalY);
          }
          runOnJS(beginPinch)();
        })
        .onUpdate((e) => {
          // Not a frame of zoom until there is something to zoom around. What
          // is on screen in the meantime is whatever was there already — most
          // often a settle from the previous pinch, still running and still
          // held by its own anchor — so the grid carries on rather than
          // stopping, and the hand is picked up at whatever scale it has
          // reached by the time the answer lands.
          if (arming.value) return;

          if (!driving.value) {
            driving.value = true;
            // Everything the gesture measures from is read on this one frame,
            // together: the cell size it is scaling away from, the scale it is
            // scaling from, and where the anchored point currently sits. Read
            // any of them earlier and they describe a grid that has moved since
            // — which is exactly what a zoom taking over from a settle still in
            // flight would be doing.
            startCell.value = valueAt(cells, z.value);
            baseScale.value = e.scale;
            const fresh = armed.value;
            if (fresh !== null) {
              armed.value = null;
              const r = rectAt(fresh.places, z.value);
              anchor.value = {
                places: fresh.places,
                frac: fresh.frac,
                screenY: r.y + fresh.frac * r.size + padTop - scrollY.value,
              };
            }
          }

          const scale = e.scale / (baseScale.value || 1);
          z.value = clamp(zoomForCell(cells, startCell.value * scale), 0, MAX_ZOOM);
        })
        // Finalize rather than end, because a gesture that is cancelled rather
        // than finished still has to give the scroll back: `pinching` is what
        // switches it off, and only `onSettled` switches it on again.
        .onFinalize(() => {
          arming.value = false;
          driving.value = false;
          armed.value = null;
          // Eased to the nearest level rather than left between two, so every
          // tile rasterises at exactly its cell size again — the difference
          // between scale(1) and scale(0.9999999).
          const to = Math.round(z.value);
          z.value = withTiming(
            to,
            { duration: SETTLE_MS, easing: Easing.out(Easing.cubic) },
            (finished) => {
              if (!finished) return;
              anchor.value = null;
              // Handed back before React hears the zoom is over, so the scroll
              // events that follow are answered again. `scrollY` rather than
              // the offset the anchor last commanded, because a pinch that was
              // cancelled before it ever moved anything never commanded one.
              zooming.value = false;
              runOnJS(onSettled)(to, scrollY.value);
            },
          );
        }),
    [
      anchor,
      armed,
      arming,
      armAnchor,
      baseScale,
      beginPinch,
      cells,
      driving,
      onSettled,
      padTop,
      scrollY,
      startCell,
      z,
      zooming,
    ],
  );

  /**
   * Every frame of a zoom: hold the anchor, and tell React when it needs to
   * hear about it.
   *
   * A reaction rather than the gesture's own `onUpdate`, because the settle at
   * the end is a `withTiming` nobody is touching — the anchor has to hold
   * through that too, or the photograph under the fingers drifts away for the
   * three hundred milliseconds after they lift.
   */
  useAnimatedReaction(
    () => z.value,
    (v) => {
      // The box the tiles are laid out at, written before anything reads it and
      // in the same flush as the transforms that divide by it. Ceiling, so a
      // tile is never laid out smaller than the cell it is being drawn at, and
      // exact at every level, so a settled grid draws at scale(1).
      boxSize.value = valueAt(cells, clamp(Math.ceil(v), 0, MAX_ZOOM));

      // Where the grid is, or is about to be. Carried to React below because
      // the scroll handler is standing down for the duration of the zoom and
      // this is the only place that knows.
      let at = scrollY.value;
      const held = anchor.value;
      if (held) {
        const r = rectAt(held.places, v);
        const point = r.y + held.frac * r.size;
        const limit = Math.max(0, valueAt(heights, v) + padTop + padBottom - viewport);
        at = clamp(point + padTop - held.screenY, 0, limit);
        scrollTo(scroller, 0, at, false);
      }
      // React is pulled back in only when the mounted set or the layout box may
      // have to change — a handful of renders per transition, not one a frame.
      if (Math.abs(v - told.value) < ZOOM_STEP) return;
      told.value = v;
      runOnJS(onZoomed)(v, at);
    },
  );

  /**
   * How tall the board is, and why it is the ceiling rather than the blend.
   *
   * The anchor above writes a scroll offset every frame of a zoom, and an
   * offset past the end of the content is one the scroll view quietly clamps to
   * whatever content it currently has. Zooming in makes the timeline taller —
   * three times taller across the scale — so a height that only ever reached
   * what the current position needs arrived a frame behind the offset that
   * needed it, and near the foot of the archive the clamp took the difference.
   * The photograph under the fingers slid away all the way in and jumped back
   * at the end.
   *
   * Reaching the level the transition is heading for means the room is always
   * already there and the clamp never fires; the anchor's own `limit` above,
   * which is the exact blend, stays the only thing bounding the scroll. Exact
   * at rest, where the ceiling of a settled level is that level — so the grid
   * still ends every gesture scrollable to precisely its own last row.
   */
  const boardStyle = useAnimatedStyle(() => ({
    height: valueAt(heights, clamp(Math.ceil(z.value), 0, MAX_ZOOM)),
  }));

  // ── The tap, the hold and the drag ─────────────────────────────────────────

  /**
   * The position under a point on the screen, or −1.
   *
   * `itemAtPoint` answers with the *nearest* tile, which is what a zoom wants —
   * it has to anchor to something wherever the fingers land. A tap, a hold and
   * a drag are the other case: they have to land *on* a photograph to mean one,
   * and the gaps between tiles and the day headings above them are not
   * photographs. So the square it names is checked rather than assumed.
   */
  const positionAt = useCallback(
    (x: number, y: number) => {
      const e = env.current;
      if (e.total === 0 || e.days.length === 0) return -1;
      const at = frameAt(e.levels, zoom.current);
      const bx = x - GUTTER;
      const by = top.current + y;
      const index = itemAtPoint(e.days, e.total, at, bx, by);
      const rect = rectAt(places(index), zoom.current);
      if (bx < rect.x || bx > rect.x + rect.size) return -1;
      if (by < rect.y || by > rect.y + rect.size) return -1;
      return index;
    },
    [places],
  );

  /**
   * The photograph under a point, as opposed to the position under it.
   *
   * A square whose photograph has not been fetched yet names a position and no
   * asset: there is nothing to enlarge and nothing for a viewer opened onto it
   * to show. A selection is the other way round — it is runs of indices
   * precisely so that it can cover ground nobody has downloaded — which is why
   * `positionAt` above stops short of asking the store.
   */
  const photoAt = useCallback(
    (x: number, y: number) => {
      const index = positionAt(x, y);
      if (index < 0) return null;
      const item = itemAt(index);
      if (!item) return null;
      return { index, item, rect: rectAt(places(index), zoom.current) };
    },
    [itemAt, places, positionAt],
  );

  /**
   * Which tile a finger is currently on, or null.
   *
   * Set the moment a finger lands and dropped when it leaves, which is what
   * draws the tile back under it — see the hold below. Held for as long as a
   * peek is open, so the photograph lifts from where the shrink left it and
   * comes back to the same place rather than to a tile that sprang out from
   * under it while the blur was up.
   */
  const [pressing, setPressing] = useState<number | null>(null);
  /** Whether the tile under the finger became a peek, so releasing keeps it. */
  const lifted = useRef(false);

  const closePeek = useCallback(() => {
    setPeek(null);
    setPressing(null);
    lifted.current = false;
  }, []);

  const openPeek = useCallback(
    (x: number, y: number) => {
      const found = photoAt(x, y);
      if (!found) return;
      lifted.current = true;
      // The square the hold has already shrunk, not the cell it sits in: the
      // lift begins from exactly what is on screen, so there is no jump
      // between the shrink ending and the photograph rising.
      const inset = (found.rect.size * (1 - PRESS_SCALE)) / 2;
      setPeek({
        item: found.item,
        index: found.index,
        from: {
          x: found.rect.x + GUTTER + inset,
          y: found.rect.y - top.current + inset,
          size: found.rect.size * PRESS_SCALE,
        },
      });
    },
    [photoAt],
  );

  /**
   * Opening the viewer, by position rather than by id.
   *
   * The position is what the viewer pages over and what the timeline is
   * addressed by, so handing it the index is handing it everything — the store
   * it reads is the one this screen published, and the photograph tapped is
   * already in it.
   */
  const openViewer = useCallback(
    (x: number, y: number) => {
      if (coasting.current) return;
      const found = photoAt(x, y);
      if (found) router.push(`/viewer/${found.index}`);
    },
    [photoAt],
  );

  /**
   * A tap, which means one of two things depending on what the grid is doing.
   *
   * Browsing, it opens the photograph. Picking, it picks or unpicks the one
   * under the finger — and never opens the viewer, because a grid in selection
   * mode that could still be navigated out of by a mis-tap is a selection
   * somebody loses by accident.
   */
  const onTap = useCallback(
    (x: number, y: number) => {
      if (coasting.current) return;
      if (!pickingNow.current) {
        openViewer(x, y);
        return;
      }
      const index = positionAt(x, y);
      if (index < 0) return;
      toggle(index);
      void Haptics.selectionAsync();
    },
    [openViewer, positionAt, toggle],
  );

  // ── The drag that picks ────────────────────────────────────────────────────
  //
  // Sideways to start, then wherever you like: the grid only ever scrolls
  // vertically, so a horizontal movement is a gesture nothing else wants.
  // Moving up or down from there runs the selection through the tiles on the
  // way — and into the band at either edge, which pulls the timeline past the
  // finger so a drag can cover more than a screen.
  //
  // Only ever *inside* selection mode. This is iOS Photos' gesture and that is
  // iOS Photos' rule: a sideways drag on a wall of photographs is somebody
  // being imprecise about a scroll far more often than it is somebody asking to
  // start choosing, and a grid that answered it by turning selection on was a
  // grid that kept changing what a tap meant behind your back. The ways in are
  // the pill and the hold's first row, both of which are somebody saying so.

  /** True from the moment the pan claims the gesture until it lets go. */
  const painting = useRef(false);
  /** Where the finger is, in screen coordinates, for the edge bands to read. */
  const finger = useRef({ x: 0, y: 0 });
  /** The last position picked, so crossing into a new tile can tick once. */
  const lastPainted = useRef(-1);
  const pulling = useRef<ReturnType<typeof setInterval> | null>(null);
  const [dragging, setDragging] = useState(false);

  const paintAt = useCallback(
    (x: number, y: number) => {
      const index = positionAt(x, y);
      if (index < 0 || index === lastPainted.current) return;
      lastPainted.current = index;
      moveDrag(index);
      void Haptics.selectionAsync();
    },
    [positionAt, moveDrag],
  );

  /**
   * One tick of the edge pull.
   *
   * The scroll offset is written here as well as read, because the handler that
   * normally keeps `top` current is the scroll view's own and does not run
   * until the frame after — and the selection this tick lays down is measured
   * against where the grid is *now*.
   */
  const pull = useCallback(() => {
    const e = env.current;
    const { x, y } = finger.current;
    if (e.height === 0) return;

    const into =
      y < EDGE_BAND ? -(1 - y / EDGE_BAND) : y > e.height - EDGE_BAND ? 1 - (e.height - y) / EDGE_BAND : 0;
    if (into === 0) return;

    const extent = valueAt(e.heights, zoom.current) + e.padTop + e.padBottom;
    const limit = Math.max(0, extent - e.height);
    const now = clamp(top.current + e.padTop + into * EDGE_SPEED, 0, limit);
    if (now === top.current + e.padTop) return;

    scroller.current?.scrollTo({ y: now, animated: false });
    top.current = now - e.padTop;
    paintAt(x, y);
  }, [paintAt, scroller]);

  const beginPaint = useCallback(
    (x: number, y: number) => {
      const index = positionAt(x, y);
      if (index < 0) return;
      painting.current = true;
      finger.current = { x, y };
      lastPainted.current = index;
      setDragging(true);
      beginDrag(index);
      void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
      pulling.current ??= setInterval(pull, 16);
    },
    [positionAt, beginDrag, pull],
  );

  const movePaint = useCallback(
    (x: number, y: number) => {
      if (!painting.current) return;
      finger.current = { x, y };
      paintAt(x, y);
    },
    [paintAt],
  );

  const endPaint = useCallback(() => {
    if (pulling.current) {
      clearInterval(pulling.current);
      pulling.current = null;
    }
    if (!painting.current) return;
    painting.current = false;
    lastPainted.current = -1;
    setDragging(false);
    endDrag();
  }, [endDrag]);

  // A grid that unmounts mid-drag would otherwise leave the interval running.
  useEffect(() => endPaint, [endPaint]);

  const paint = useMemo(
    () =>
      Gesture.Pan()
        // Off entirely while the grid is browsing, so a sideways thumb on a
        // photograph does nothing at all rather than something nobody asked
        // for.
        .enabled(picking)
        // Sideways to claim it, and a vertical start hands it straight back to
        // the scroll view. Fourteen points is far enough that a thumb drifting
        // on its way to a flick never trips it.
        .activeOffsetX([-14, 14])
        .failOffsetY([-14, 14])
        // The gesture only activates once the finger has already moved, so the
        // tile it began on is where it was before that movement — not where it
        // is now, which is a cell or two along.
        .onStart((e) => {
          runOnJS(beginPaint)(e.x - e.translationX, e.y - e.translationY);
        })
        .onUpdate((e) => {
          runOnJS(movePaint)(e.x, e.y);
        })
        // Finalize rather than end: a gesture cancelled by a second finger
        // coming down still has to commit the run it laid and stop pulling.
        .onFinalize(() => {
          runOnJS(endPaint)();
        }),
    [picking, beginPaint, movePaint, endPaint],
  );

  /**
   * What the hold offers, for the one photograph it lifted.
   *
   * Every row closes the peek first, and two of them have to: the album picker
   * and the create-album form are sheets drawn by the app, and the peek is a
   * `Modal` drawn over it — a sheet opened from in here would come up behind
   * the very thing that asked for it. See src/actions/filing.
   *
   * Which rows there are is the scope's to decide, exactly as it is for the
   * selection sheet: in the library a photograph can be filed, archived, hidden
   * or deleted; in Recently Deleted it can come back or go for good; in the
   * vault it can only come back out.
   */
  const peekActions = useMemo<Action[]>(() => {
    if (!peek) return [];
    const { item, index } = peek;
    const noun = nounFor(item.kind);
    const one = { kind: 'items', count: 1, noun } as const;
    const target = { ids: [item.id] };
    const picked = selected(index);
    const shut = closePeek;

    const rows: Action[] = [
      {
        key: 'select',
        label: picked ? 'Deselect' : 'Select',
        icon: picked ? 'x-circle' : 'check-circle',
        onPress: () => {
          shut();
          if (!pickingNow.current) enter();
          toggle(index);
        },
      },
    ];

    if (actions.scope === 'trash' || actions.scope === 'vault') {
      rows.push({
        key: 'restore',
        label:
          actions.scope === 'vault'
            ? describeAction(actions.bucket === 'hidden' ? 'Unhide' : 'Unarchive', one)
            : 'Restore',
        icon: 'rotate-ccw',
        onPress: () => {
          shut();
          void actions.restore(target, noun);
        },
      });
    }

    if (actions.scope !== 'trash') {
      rows.push({
        key: 'file',
        label: 'Add to album',
        icon: 'folder-plus',
        onPress: () => {
          shut();
          askToFile({ target, noun, assetId: item.id });
        },
      });
    }

    // Not armed, and not last. Filing something away is undoable from the
    // notice and reversible from a screen one tap away; only the delete below
    // is buying two taps with the thing it cannot buy back. The order says so.
    //
    // Neither needs the vault unlocked. Putting a photograph in works on a
    // locked vault and creates one that does not exist yet — which is the whole
    // asymmetry the feature rests on, and why this is a menu row rather than a
    // password prompt. `useTrashActions` hands the two states that do need a
    // password to the gate instead of to a notice; see core's `needsVault`.
    if (actions.scope === 'library') {
      rows.push(
        {
          key: 'archive',
          label: describeAction('Archive', one),
          icon: 'archive',
          onPress: () => {
            shut();
            void actions.hide('archive', target, noun);
          },
        },
        {
          key: 'hide',
          label: describeAction('Hide', one),
          icon: 'eye-off',
          onPress: () => {
            shut();
            void actions.hide('hidden', target, noun);
          },
        },
      );
    }

    const album = actions.scope === 'trash' ? undefined : actions.album;
    if (album) {
      rows.push({
        key: 'unfile',
        label: `${describeAction('Remove', one)} from album`,
        icon: 'folder-minus',
        armed: true,
        onPress: () => {
          shut();
          void actions.unfile(album, target, noun);
        },
      });
    }

    if (actions.scope !== 'vault') {
      rows.push({
        key: 'delete',
        label: describeAction(actions.scope === 'trash' ? 'Delete forever' : 'Delete', one),
        icon: 'trash-2',
        tone: 'destructive',
        armed: true,
        onPress: () => {
          shut();
          void (actions.scope === 'trash'
            ? actions.purge(target, noun)
            : actions.remove(target, noun));
        },
      });
    }

    return rows;
  }, [peek, actions, selected, enter, toggle, closePeek]);

  /**
   * A finger has landed. Whichever tile it is on begins to answer.
   *
   * The answer is deliberately late — see `PRESS_LEAD_MS` — so a tap and the
   * start of a scroll both come and go without the grid twitching. What is
   * begun here is only the knowledge of *which* tile; the shape of the shrink
   * belongs to the tile itself, which is what keeps it off this thread.
   */
  const beginHold = useCallback(
    (x: number, y: number) => {
      const index = positionAt(x, y);
      setPressing(index < 0 ? null : index);
    },
    [positionAt],
  );

  /**
   * The finger has gone, or another gesture has taken it.
   *
   * Not while a peek is up: the photograph lifted out of that tile and has to
   * be able to fall back into the same square, so the shrink is the peek's to
   * release. Everywhere else the tile comes back on its own.
   */
  const endHold = useCallback(() => {
    if (lifted.current) return;
    setPressing(null);
  }, []);

  const hold = useMemo(
    () =>
      Gesture.LongPress()
        .minDuration(HOLD_MS)
        // Enough that a finger settling on the way to a scroll never trips it,
        // little enough that it is a hold rather than a wait.
        .maxDistance(12)
        .onBegin((e) => {
          runOnJS(beginHold)(e.x, e.y);
        })
        .onStart((e) => {
          runOnJS(openPeek)(e.x, e.y);
        })
        // Finalize rather than end, because the common case is this gesture
        // losing: a scroll or a pinch claiming the finger cancels the hold, and
        // a tile left drawn back under a finger that has moved on is a tile
        // nobody can explain.
        .onFinalize(() => {
          runOnJS(endHold)();
        }),
    [beginHold, endHold, openPeek],
  );

  const tap = useMemo(
    () =>
      Gesture.Tap().onEnd((e, ok) => {
        if (ok) runOnJS(onTap)(e.x, e.y);
      }),
    [onTap],
  );

  // Whichever wins: two fingers is a zoom, one finger dragged sideways picks a
  // run, one finger held still is a peek, one finger down and up is a
  // photograph opened or chosen, and no gesture is ever two.
  const gestures = useMemo(
    () => Gesture.Race(pinch, paint, hold, tap),
    [pinch, paint, hold, tap],
  );

  // ── Fetching ───────────────────────────────────────────────────────────────

  useEffect(() => {
    if (viewport === 0) return;
    request('grid', wanted.start, wanted.end);
  }, [request, wanted.start, wanted.end, viewport]);

  // A reload makes every position mean a different photograph, so what is
  // mounted has to be recomputed against the table that just landed rather than
  // the one the last scroll was measured against.
  useLayoutEffect(() => {
    settleRange(false);
    settlePill();
  }, [days, total, viewport, settleRange, settlePill]);

  const visible = useMemo(() => {
    const out: number[] = [];
    const end = Math.min(window.end, total);
    for (let index = Math.max(0, window.start); index < end; index++) out.push(index);
    return out;
  }, [window.start, window.end, total]);

  /**
   * Every mounted square, and whatever has been fetched for it.
   *
   * `timeline` is a dependency because `itemAt` reads through a ref: the store
   * holds one slot per photograph in the archive and mutates it in place, since
   * copying that array to announce that a page of two hundred landed would be
   * the most expensive thing in the gallery. Its own identity is the only thing
   * React can see change.
   *
   * Which makes this the seam the React Compiler needs — it is on for the phone
   * and off for the browser, and given `visible` and a stable `itemAt` it would
   * otherwise be entitled to hold the whole grid still while pages arrived
   * underneath it. Saying the dependency out loud is better than opting out of
   * the compiler, and it is true either way.
   */
  const squares = useMemo(
    () => visible.map((index) => ({ index, item: itemAt(index) })),
    [visible, itemAt, timeline],
  );

  // A Live Photo's motion is queued separately from its thumbnail, so a tile
  // can be drawn and still be waiting to come to life. Placeholders are not in
  // here: there is nothing to ask about an asset we have not been sent yet.
  const pendingKey = useMemo(() => {
    const ids: string[] = [];
    for (const { item } of squares) {
      if (item && (item.state === 'pending' || item.live === 'pending')) ids.push(item.id);
    }
    return ids.join(',');
  }, [squares]);

  // Only tiles actually on screen are polled. During a backfill the library can
  // be tens of thousands of pending items, and asking about all of them every
  // few seconds would cost more than generating the thumbnails.
  useEffect(() => {
    if (!pendingKey) return;
    const ids = pendingKey.split(',');
    const controller = new AbortController();
    const id = setInterval(() => {
      fetchStates(ids, controller.signal)
        .then(patch)
        .catch(() => {
          // A dropped poll is not worth surfacing; the next tick asks again.
        });
    }, POLL_MS);
    return () => {
      controller.abort();
      clearInterval(id);
    };
  }, [pendingKey, patch]);

  const headings = useMemo(() => headingsFor(days, window), [days, window]);

  return (
    <View
      style={styles.root}
      onLayout={(e) => {
        const height = e.nativeEvent.layout.height;
        setViewport((held) => (held === height ? held : height));
      }}
    >
      <GestureDetector gesture={gestures}>
        <Animated.ScrollView
          ref={scroller}
          onScroll={scrollHandler}
          // The last word on where the scroll ended, whichever way it ended.
          // The animated handler above declines to report anything while a
          // zoom owns the offset, and a fling that was still running when two
          // fingers came down comes to rest inside that window. These two are
          // ungated, so what is mounted is always settled against where the
          // grid actually is rather than against where the zoom left it.
          onScrollEndDrag={(e) => onScrolled(e.nativeEvent.contentOffset.y)}
          onMomentumScrollBegin={() => {
            coasting.current = true;
          }}
          onMomentumScrollEnd={(e) => {
            coasting.current = false;
            onScrolled(e.nativeEvent.contentOffset.y);
          }}
          scrollEventThrottle={16}
          // A deceleration still running underneath the zoom would be fighting
          // it for the same number, and the scroll view's own pan would be
          // fighting the drag that picks for the same finger.
          scrollEnabled={!pinching && !dragging && peek === null}
          showsVerticalScrollIndicator={false}
          contentContainerStyle={{
            paddingTop: padTop,
            paddingBottom: padBottom,
            paddingHorizontal: GUTTER,
          }}
        >
          <Animated.View style={boardStyle}>
            {headings.map((d) => (
              <Heading key={days[d].id} day={days[d]} tops={tops(d)} heights={headerHeights} z={z} />
            ))}

            {/* Every square in range is drawn, whether or not there is a photo
                for it yet. The two cases share one geometry — the index decides
                where the square is, and only what goes inside it depends on
                having been fetched. */}
            {squares.map(({ index, item }) =>
              item ? (
                <Tile
                  key={item.id}
                  item={item}
                  box={boxSize}
                  thumb={thumb}
                  places={places(index)}
                  z={z}
                  cache={cache}
                  selected={picking && selected(index)}
                  pressed={pressing === index}
                />
              ) : (
                <Skeleton
                  key={`@${index}`}
                  box={boxSize}
                  places={places(index)}
                  z={z}
                  selected={picking && selected(index)}
                  pressed={pressing === index}
                />
              ),
            )}
          </Animated.View>
        </Animated.ScrollView>
      </GestureDetector>

      {/* The grid runs to the top edge, the way a photo app's does, which means
          photographs pass behind the clock and the battery. Under a blur they
          read as passing behind something; under nothing they read as broken.
          It is the same strip iOS puts there itself, and the reason expo-blur
          is a dependency at all. */}
      {immersive ? (
        <BlurView
          intensity={28}
          tint="dark"
          style={[styles.statusScrim, { height: insets.top }]}
          pointerEvents="none"
        />
      ) : null}

      <DatePill
        label={pinned.label}
        visible={pinned.visible}
        top={(immersive ? insets.top : 0) + space.xs}
      />

      <Scrubber
        scroller={scroller}
        scrollY={scrollY}
        z={z}
        heights={heights}
        chrome={padTop + padBottom}
        viewport={viewport}
        label={pinned.label}
        top={(immersive ? insets.top : 0) + space.xxl}
        bottom={padBottom}
      />

      {/* Two different sentences, and the difference is whether there is a
          grid behind this one. With a cached day table painted, an archive out
          of reach is not a failure to report in red — the geometry is right,
          the ground already walked is drawn, and what is missing is only
          whatever has changed since. Without one there is nothing on screen at
          all, and that is the error it has always been. */}
      {error ? (
        <View style={[styles.notice, { bottom: padBottom }]}>
          <Text
            variant="small"
            tone={stale ? 'muted' : 'destructive'}
            style={styles.noticeText}
          >
            {stale ? `${error}. Showing what was here last time.` : error}
          </Text>
          <Button label="Retry" onPress={retry} />
        </View>
      ) : null}

      {/* No "loading" notice: the grid is already the right size and full of the
          squares the photographs are going to land in, which says it better than
          a line of text could. An empty archive is a different statement and is
          the one worth making. */}
      {ready && !loading && total === 0 && !error ? (
        <View style={styles.empty} pointerEvents="none">
          <Text variant="small" tone="muted" style={styles.emptyText}>
            {empty ?? 'Nothing here yet. Run a backup from the Backup tab.'}
          </Text>
        </View>
      ) : null}

      <Peek target={peek} actions={peekActions} cache={cache} onClose={closePeek} />
    </View>
  );
}

/**
 * One day's heading, placed by the same blend the tiles are.
 *
 * Its height is blended too, and it has to be: a heading is 52 points at every
 * level, but the *gap* between the day above and the tiles below it is part of
 * what `layoutLevel` stacked, so a fixed box here would leave the label sitting
 * at the wrong end of its own space halfway through a transition.
 */
function Heading({
  day,
  tops,
  heights,
  z,
}: {
  day: Day;
  tops: number[];
  heights: number[];
  z: SharedValue<number>;
}) {
  const style = useAnimatedStyle(() => ({
    transform: [{ translateY: valueAt(tops, z.value) }],
    height: valueAt(heights, z.value),
  }));

  return (
    <Animated.View style={[styles.heading, style]} pointerEvents="none">
      <Text variant="title" numberOfLines={1} style={styles.headingLabel}>
        {day.label}
      </Text>
      <Text variant="caption" tone="faint">
        {day.count}
      </Text>
    </Animated.View>
  );
}

/** Every heading that could be on screen: the mounted days, plus the next one down. */
function headingsFor(days: Day[], range: ItemRange): number[] {
  if (days.length === 0 || range.end <= range.start) return [];
  const first = dayIndexOf(days, Math.max(0, range.start));
  const last = Math.min(dayIndexOf(days, range.end - 1) + 1, days.length - 1);
  const out: number[] = [];
  for (let d = first; d <= last; d++) out.push(d);
  return out;
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: color.background },
  statusScrim: { position: 'absolute', top: 0, left: 0, right: 0 },
  heading: {
    position: 'absolute',
    top: 0,
    left: 0,
    right: 0,
    flexDirection: 'row',
    alignItems: 'flex-end',
    gap: space.sm,
    paddingBottom: space.sm,
  },
  headingLabel: { flexShrink: 1 },
  notice: {
    position: 'absolute',
    left: space.lg,
    right: space.lg,
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    backgroundColor: color.card,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    borderRadius: radius.lg,
    paddingHorizontal: space.md,
    paddingVertical: space.sm,
  },
  noticeText: { flex: 1 },
  empty: {
    position: 'absolute', top: 0, left: 0, right: 0, bottom: 0,
    alignItems: 'center',
    justifyContent: 'center',
    paddingHorizontal: space.xl,
    paddingBottom: TAB_BAR_CLEARANCE,
  },
  emptyText: { textAlign: 'center' },
});
