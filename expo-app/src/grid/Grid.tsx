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
import { Skeleton, Tile } from './Tile';
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

/** Room above the first heading, under the status bar and the floating date. */
const TOP_ROOM = space.xxl;

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
  const { days, total, ready, loading, error, retry, at: itemAt, request, patch } = timeline;

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
        layoutLevel(days, metricsFor(inner, cap, flat ? { headerHeight: 0 } : {})),
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
   * The size tiles are laid out at, and the rendition drawn into them.
   *
   * The box follows the *largest* level a running transition will reach, so
   * zooming in upgrades the picture as soon as the tiles are laid out for it,
   * and zooming out keeps the larger one until the grid has settled at the
   * smaller cell. Between gestures it is simply the settled level.
   */
  const [level, setLevel] = useState(() => Math.round(z.value));
  const [boxLevel, setBoxLevel] = useState(level);
  const box = cells[boxLevel] ?? cells[level] ?? 1;
  const thumb = thumbSizeFor(boxLevel);

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
    (v: number) => {
      zoom.current = v;
      // Only ever upwards during a gesture: tiles are laid out at this box and
      // scaled down from it, so a box that shrank mid-transition would be
      // stretching a small picture across a large cell.
      setBoxLevel((held) => Math.max(held, clamp(Math.ceil(v), 0, MAX_ZOOM)));
      settleRange(true);
      settlePill();
    },
    [settleRange, settlePill],
  );

  const onSettled = useCallback(
    (to: number) => {
      zoom.current = to;
      setLevel(to);
      setBoxLevel(to);
      setPinching(false);
      settleRange(false);
      settlePill();
      rememberLevel(to);
    },
    [settleRange, settlePill],
  );

  const scrollHandler = useAnimatedScrollHandler({
    onScroll: (e) => {
      scrollY.value = e.contentOffset.y;
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
  const anchor = useSharedValue<{
    /** Where the anchored tile sits at each level — all the UI thread needs. */
    places: number[];
    /** How far down that tile the held point is, 0–1. */
    frac: number;
    /** Where the held point should stay, measured from the top of the screen. */
    screenY: number;
  } | null>(null);
  const startCell = useSharedValue(0);
  const told = useSharedValue(z.value);

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
      setPinching(true);
      const e = env.current;
      if (e.total === 0 || e.days.length === 0) {
        anchor.value = null;
        return;
      }
      const at = frameAt(e.levels, zoom.current);
      const y = top.current + focalY;
      const index = itemAtPoint(e.days, e.total, at, focalX - GUTTER, y);
      const held = places(index);
      const rect = rectAt(held, zoom.current);
      anchor.value = {
        places: held,
        frac: clamp((y - rect.y) / (rect.size || 1), 0, 1),
        screenY: focalY,
      };
    },
    [anchor, places],
  );

  const pinch = useMemo(
    () =>
      Gesture.Pinch()
        // On activation rather than on the second finger touching down: the
        // focal point is only meaningful once the gesture has decided it is a
        // pinch, and anchoring to (0, 0) would zoom around the corner of the
        // screen. The round trip to the JS thread costs a frame or two at a
        // scale of about 1, which is nothing to look at.
        .onStart((e) => {
          startCell.value = valueAt(cells, z.value);
          runOnJS(armAnchor)(e.focalX, e.focalY);
        })
        .onUpdate((e) => {
          z.value = clamp(zoomForCell(cells, startCell.value * e.scale), 0, MAX_ZOOM);
        })
        // Finalize rather than end, because a gesture that is cancelled rather
        // than finished still has to give the scroll back: `pinching` is what
        // switches it off, and only `onSettled` switches it on again.
        .onFinalize(() => {
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
              runOnJS(onSettled)(to);
            },
          );
        }),
    [anchor, armAnchor, cells, onSettled, startCell, z],
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
      const held = anchor.value;
      if (held) {
        const r = rectAt(held.places, v);
        const at = r.y + held.frac * r.size;
        const limit = Math.max(0, valueAt(heights, v) + padTop + padBottom - viewport);
        scrollTo(scroller, 0, clamp(at + padTop - held.screenY, 0, limit), false);
      }
      // React is pulled back in only when the mounted set or the layout box may
      // have to change — a handful of renders per transition, not one a frame.
      if (Math.abs(v - told.value) < ZOOM_STEP) return;
      told.value = v;
      runOnJS(onZoomed)(v);
    },
  );

  const boardStyle = useAnimatedStyle(() => ({ height: valueAt(heights, z.value) }));

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

  const openPeek = useCallback(
    (x: number, y: number) => {
      const found = photoAt(x, y);
      if (!found) return;
      setPeek({
        item: found.item,
        index: found.index,
        from: {
          x: found.rect.x + GUTTER,
          y: found.rect.y - top.current,
          size: found.rect.size,
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
  // vertically, so a horizontal movement is a gesture nothing else wants and
  // the one unambiguous way to begin picking without first going somewhere to
  // turn picking on. Once it has begun, moving up or down runs the selection
  // through the tiles on the way — and into the band at either edge, which
  // pulls the timeline past the finger so a drag can cover more than a screen.
  //
  // This is iOS Photos' gesture, and it is the reason the hold is left alone to
  // go on meaning "show me this one".

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
      // A drag that begins outside selection mode turns it on, which is the
      // whole point of the gesture: it is the way in, not merely a faster way
      // of doing what the pill already offers.
      if (!pickingNow.current) enter();
      beginDrag(index);
      void Haptics.impactAsync(Haptics.ImpactFeedbackStyle.Light);
      pulling.current ??= setInterval(pull, 16);
    },
    [positionAt, enter, beginDrag, pull],
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
    [beginPaint, movePaint, endPaint],
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
   * vault it can only come back out. Archive and Hide are drawn and inert until
   * Phase 6 brings the gate that would let them run.
   */
  const peekActions = useMemo<Action[]>(() => {
    if (!peek) return [];
    const { item, index } = peek;
    const noun = nounFor(item.kind);
    const one = { kind: 'items', count: 1, noun } as const;
    const target = { ids: [item.id] };
    const picked = selected(index);
    const shut = () => setPeek(null);

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

    if (actions.scope === 'library') {
      rows.push(
        {
          key: 'archive',
          label: describeAction('Archive', one),
          icon: 'archive',
          disabled: true,
          note: 'Phase 6',
        },
        {
          key: 'hide',
          label: describeAction('Hide', one),
          icon: 'eye-off',
          disabled: true,
          note: 'Phase 6',
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
  }, [peek, actions, selected, enter, toggle]);

  const hold = useMemo(
    () =>
      Gesture.LongPress()
        .minDuration(380)
        // Enough that a finger settling on the way to a scroll never trips it,
        // little enough that it is a hold rather than a wait.
        .maxDistance(12)
        .onStart((e) => {
          runOnJS(openPeek)(e.x, e.y);
        }),
    [openPeek],
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
          // The animated handler above skips anything within SCROLL_STEP of
          // what it last reported, which is what keeps a fling from flooding
          // the JS thread — and which means the frame a fling comes to rest on
          // may be one it declined to mention. These two are ungated, so what
          // is mounted is always settled against where the grid actually is.
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
                  box={box}
                  thumb={thumb}
                  places={places(index)}
                  z={z}
                  selecting={picking}
                  selected={picking && selected(index)}
                />
              ) : (
                <Skeleton
                  key={`@${index}`}
                  box={box}
                  places={places(index)}
                  z={z}
                  selecting={picking}
                  selected={picking && selected(index)}
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

      {error ? (
        <View style={[styles.notice, { bottom: padBottom }]}>
          <Text variant="small" tone="destructive" style={styles.noticeText}>
            {error}
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

      <Peek target={peek} actions={peekActions} onClose={() => setPeek(null)} />
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
