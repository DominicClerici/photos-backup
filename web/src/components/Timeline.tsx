"use client";

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
} from "react";

import { Archive, EyeOff, ListMinus, RotateCcw, SquareCheck, Trash2 } from "lucide-react";

import { fetchStates, type Bucket, type CreatedAlbum } from "@/lib/api";
import type { TimelineState } from "@/hooks/useTimeline";
import {
  dayAt,
  dayIndexOf,
  frameAt,
  headerY,
  headless,
  itemAtPoint,
  itemTop,
  layoutLevel,
  metricsFor,
  thumbSizeFor,
  tileRect,
  visibleItems,
  DEFAULT_ZOOM,
  MAX_ZOOM,
  ZOOM_LEVELS,
  type Day,
  type Frame,
  type ItemRange,
} from "@/lib/layout";
import { useView, useViewScope } from "@/hooks/useView";
import { isFiltered, viewKey } from "@/lib/view";
import { Zoom, savedZoom } from "@/lib/zoom";
import {
  useSelection,
  useSelectionScope,
  type AlbumRef,
  type SelectionActions,
} from "@/hooks/useSelection";
import { Button } from "@/components/ui/button";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu";
import { ArmedMenuItem } from "./Armed";
import { AddToAlbumSub } from "./AlbumPicker";
import { CreateAlbumDialog, type CreateAlbumRequest } from "./CreateAlbumDialog";
import { counted, describeAction, nounFor } from "@/lib/format";
import { toast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";
import { Skeleton, Tile } from "./Tile";
import { ZoomSlider } from "./ZoomSlider";

/** How often unfinished tiles ask whether their derivative landed. */
const POLL_MS = 4000;
/**
 * Fetched above and below the viewport, as a fraction of its height.
 *
 * Wider than what is rendered, because a placeholder that is already on screen
 * has missed its chance to be a photograph. Scrolling at a normal speed spends
 * about a second crossing this much grid, which is roughly what a page costs.
 */
const FETCH_OVERSCAN = 2.5;
/** Rendered above and below the viewport at rest, as a fraction of its height. */
const OVERSCAN = 1;
/** Less while zooming: the far end of the range holds seven times as many tiles. */
const ZOOM_OVERSCAN = 0.35;
/** How far a ctrl-wheel gesture has to travel to buy each further level. */
const WHEEL_PER_LEVEL = 150;
/** No ctrl-wheel event for this long ends the gesture and re-anchors the travel. */
const WHEEL_IDLE_MS = 220;
/**
 * Pixels per line for wheels that report in lines rather than pixels.
 *
 * Firefox sends three lines per detent where Chrome sends 100 pixels, so this
 * is what makes one notch of the same wheel cost the same zoom in both.
 */
const WHEEL_LINE_PX = 33;

/**
 * How long a press has to rest on one tile before it turns into a selection.
 *
 * Long enough that flicking the grid or setting the pointer down on the way to
 * a click never trips it, short enough that holding a photograph to pick it is
 * one gesture rather than a wait.
 */
const LONG_PRESS_MS = 550;
/** How far a press may wander and still count as a hold rather than a drag. */
const PRESS_SLOP = 8;
/**
 * The band at each end of the grid where a selection drag scrolls the page.
 *
 * Dragging a selection is the one gesture that cannot also scroll — the pointer
 * is already saying which tiles it means — so the grid has to move itself, and
 * it has to be reachable without leaving the window.
 */
const EDGE_BAND = 96;
/** Pixels per frame at the very edge, falling to nothing at the band's inner lip. */
const EDGE_SPEED = 30;

const NOTICE =
  "flex items-center justify-center gap-3 px-3 py-7 text-sm text-muted-foreground";

/**
 * A pointer held down on the grid, before anyone knows what it means.
 *
 * It could be a click that opens a photograph, a hold that starts a selection,
 * a drag that extends one, or a finger about to scroll. Kept in a ref rather
 * than in state because every field is read by the frame loop below, which runs
 * between renders and must see where the pointer is now.
 */
interface Press {
  id: number;
  /** The tile it went down on: where a drag measures from. */
  index: number;
  /** Where it went down, which is what "has it wandered yet" is measured from. */
  fromX: number;
  fromY: number;
  /** Live client coordinates, so the autoscroll loop can re-read them. */
  x: number;
  y: number;
  touch: boolean;
  /** Whether it has become a selection drag. */
  dragging: boolean;
  /** Whether the hold has already fired, so the release must not pick again. */
  held: boolean;
  moved: boolean;
  timer: number;
}

/** A mounted element, tagged with everything the frame loop needs to place it. */
interface Slot {
  el: HTMLElement;
  day: number;
  offset: number;
}

/**
 * The point held still while the grid re-flows underneath it.
 *
 * A tile rather than a scroll offset, because the offset means something
 * different at every zoom level: the whole timeline is nearly three times taller
 * at the smallest cell size than at the largest.
 */
interface Anchor {
  day: number;
  offset: number;
  /** How far down that tile the held point sits, 0–1. */
  frac: number;
  /** Where the held point should stay, measured from the top of the viewport. */
  screenY: number;
}

interface Props {
  /**
   * The collection being browsed. Passed whole rather than spread into props:
   * the geometry, the items and the request that fetches them are one thing,
   * and the grid reads all three on the same frame.
   */
  timeline: TimelineState;
  /**
   * What can be done to a selection of this collection.
   *
   * Passed in rather than built here because the grid does not know which
   * timeline it is drawing — it is handed one, filtered or not — and every one
   * of these actions has to name that filter to mean anything. The grid's job
   * is to publish them to the floating control and to spend them from its own
   * menu; deciding what they are belongs to whoever chose the collection.
   */
  actions: SelectionActions;
  onOpen: (id: string) => void;
}

export function Timeline({ timeline, actions, onOpen }: Props) {
  const { days, total, ready, loading, error, retry, at: itemAt, request, patch } = timeline;
  const scroller = useRef<HTMLDivElement>(null);
  const board = useRef<HTMLDivElement>(null);
  const pill = useRef<HTMLDivElement>(null);

  const [size, setSize] = useState({ width: 0, height: 0 });
  const [mounted, setMounted] = useState(false);
  const [tick, invalidate] = useReducer((n: number) => n + 1, 0);

  const held = useRef<Zoom>(null);
  held.current ??= new Zoom(savedZoom());
  const zoom = held.current;
  useEffect(() => () => zoom.dispose(), [zoom]);
  useEffect(() => setMounted(true), []);

  // A timeline with no days reserves no room for the headings it will not
  // draw, which turns the grid into the flat wall of tiles an order by length
  // actually is. Everything below is unchanged by it: one day, one heading of
  // no height, every tile hanging under it.
  const flat = headless(days);
  const levels = useMemo(
    () =>
      ZOOM_LEVELS.map((cap) =>
        layoutLevel(days, metricsFor(size.width, cap, flat ? { headerHeight: 0 } : {})),
      ),
    [days, size.width, flat],
  );

  // Everything the per-frame loop reads, refreshed from each render by the
  // layout effect below. During a zoom the loop runs without React: re-rendering
  // a screenful of tiles sixty times a second would cost more than the animation
  // it is trying to draw, so React owns *which* tiles exist and the loop owns
  // where they sit.
  const env = useRef({
    days,
    levels,
    count: total,
    height: 0,
    box: 1,
    boxLevel: DEFAULT_ZOOM,
    scrollTop: 0,
    /** The last scroll offset this component wrote, to tell ours from the user's. */
    applied: -1,
    window: null as ItemRange | null,
    anchor: null as Anchor | null,
    tiles: [] as Slot[],
    heads: [] as Slot[],
  });

  // Read during render rather than held in state. The zoom advances every frame
  // and the grid must agree with it on frames React never sees, so the animated
  // value is the single source and both paths read it directly.
  const z = zoom.value;
  const settled = !zoom.moving;
  const frame = frameAt(levels, z);
  const boxLevel = clamp(settled ? Math.round(zoom.target) : env.current.boxLevel, 0, MAX_ZOOM);
  const box = levels[boxLevel].metrics.cellSize;
  // The rendition follows the box for the same reason the box exists: it is the
  // largest size this transition will draw, so zooming in upgrades the picture
  // as soon as the tiles are laid out for it, and zooming out keeps the larger
  // one until the grid has settled at the smaller cell.
  const thumb = thumbSizeFor(boxLevel);

  const needed = visibleItems(
    days,
    total,
    frame,
    env.current.scrollTop,
    size.height,
    size.height * (settled ? OVERSCAN : ZOOM_OVERSCAN),
  );
  const range = settled ? needed : (env.current.window ?? needed);

  const updatePill = useCallback(
    (at?: Frame) => {
      const label = pill.current;
      const e = env.current;
      if (!label) return;
      const on = at ?? frameAt(e.levels, zoom.value);
      const day = dayAt(e.days, on, e.scrollTop);
      // Show the floating date only once its own heading has scrolled away, so
      // it never sits directly above a heading saying the same thing. A day
      // with no label is a timeline with no days, and there is no date to float.
      const show = day != null && day.label !== "" && e.scrollTop > day.top + on.headerHeight;
      label.style.opacity = show ? "1" : "0";
      if (day) label.textContent = day.label;
    },
    [zoom],
  );

  /** Pins the tile under a point in the viewport, keeping that point where it is. */
  const anchorAt = useCallback(
    (clientX: number, clientY: number) => {
      const el = scroller.current;
      const canvas = board.current;
      const e = env.current;
      if (!el || !canvas || e.count === 0) {
        e.anchor = null;
        return;
      }
      const rect = canvas.getBoundingClientRect();
      const x = clientX - rect.left;
      const y = clientY - rect.top;

      const at = frameAt(e.levels, zoom.value);
      const index = itemAtPoint(e.days, e.count, at, x, y);
      const day = dayIndexOf(e.days, index);
      const offset = index - e.days[day].start;
      const r = tileRect(at, day, offset);

      e.anchor = {
        day,
        offset,
        frac: clamp((y - r.y) / (r.size || 1), 0, 1),
        screenY: y - el.scrollTop,
      };
      e.applied = el.scrollTop;
    },
    [zoom],
  );

  const anchorCentre = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    anchorAt(rect.left + rect.width / 2, rect.top + rect.height / 2);
  }, [anchorAt]);

  /**
   * One frame of zoom: the scroll offset, the spacer, and every tile's place.
   *
   * Only transforms are written. The tiles are already laid out at the largest
   * box they will reach during this transition, so scaling them down is a
   * composite rather than a re-layout and a fresh rasterisation of every
   * thumbnail on screen.
   */
  const applyFrame = useCallback(() => {
    const el = scroller.current;
    const canvas = board.current;
    const e = env.current;
    if (!el || !canvas) return;

    const moving = zoom.moving;
    if (moving && !e.anchor) anchorCentre();

    const at = frameAt(e.levels, zoom.value);
    canvas.style.height = `${at.totalHeight}px`;

    const anchor = e.anchor;
    if (anchor && e.count > 0) {
      const held = tileRect(at, anchor.day, anchor.offset).y + anchor.frac * at.cellSize;
      // Scrolling during a zoom wins: adopt where the user went and re-hang the
      // anchor there rather than dragging them back to it.
      if (e.applied >= 0 && Math.abs(el.scrollTop - e.applied) > 0.5) {
        anchor.screenY = held - el.scrollTop;
      }
      const limit = Math.max(0, at.totalHeight - e.height);
      el.scrollTop = clamp(held - anchor.screenY, 0, limit);
      e.applied = el.scrollTop;
    }
    e.scrollTop = el.scrollTop;

    for (const slot of e.tiles) {
      const r = tileRect(at, slot.day, slot.offset);
      slot.el.style.transform = `translate(${r.x}px,${r.y}px) scale(${r.size / e.box})`;
    }
    for (const slot of e.heads) {
      slot.el.style.transform = `translateY(${headerY(at, slot.day)}px)`;
    }
    updatePill(at);

    // React is only pulled back in when the set of mounted tiles no longer
    // covers the screen, or when the boxes they sit in are about to be too
    // small to scale down from — a handful of renders per transition, not one
    // per frame.
    let stale = false;
    if (moving) {
      const need = visibleItems(
        e.days,
        e.count,
        at,
        e.scrollTop,
        e.height,
        e.height * ZOOM_OVERSCAN,
      );
      const win = e.window;
      if (!win) {
        e.window = need;
        e.boxLevel = boxLevelFor(zoom);
        stale = true;
      } else {
        if (need.start < win.start || need.end > win.end) {
          e.window = {
            start: Math.min(win.start, need.start),
            end: Math.max(win.end, need.end),
          };
          stale = true;
        }
        const want = boxLevelFor(zoom);
        if (want > e.boxLevel) {
          e.boxLevel = want;
          stale = true;
        }
      }
    } else if (e.window) {
      // Settled: hand the grid back at its native cell size, with no scale left
      // on it, so the thumbnails rasterise sharp again.
      e.window = null;
      e.anchor = null;
      stale = true;
    }
    if (stale) invalidate();
  }, [anchorCentre, updatePill, zoom]);

  useEffect(() => zoom.subscribe(applyFrame), [applyFrame, zoom]);

  // Selection is addressed in item indices, which is what lets it cover ground
  // the client is not holding: the day table gives every photograph in the
  // collection a place before any of them are fetched, so a drag can run
  // through five thousand tiles that are still placeholders and mean exactly
  // the five thousand photographs those squares stand for.
  const {
    active: selecting,
    selected,
    count: picked,
    ranges,
    enter,
    exit,
    toggle,
    span,
    beginDrag,
    moveDrag,
    endDrag,
  } = useSelection();
  useSelectionScope(actions);

  /**
   * Puts a position at the top of the viewport, which is what jumping to a date
   * is once the day table has turned that date into a number.
   *
   * The heading comes with it when the position is the first of its day —
   * landing on the first tile of March with "March" scrolled off the top would
   * be arriving somewhere without being told where. Read out of the geometry
   * rather than from an element, because the tile being jumped to is almost
   * never mounted: it is thirty thousand squares away.
   */
  const jump = useCallback(
    (index: number) => {
      const el = scroller.current;
      const e = env.current;
      if (!el || e.count === 0) return;

      const at = frameAt(e.levels, zoom.value);
      const day = dayIndexOf(e.days, clamp(index, 0, e.count - 1));
      const top =
        index === e.days[day].start
          ? headerY(at, day)
          : itemTop(e.days, at, index) - at.headerHeight;

      el.scrollTop = clamp(top, 0, Math.max(0, at.totalHeight - e.height));
      e.scrollTop = el.scrollTop;
      e.applied = el.scrollTop;
      invalidate();
    },
    [zoom],
  );

  // What the sort-and-filter pill needs to act on this grid: what it is a grid
  // of, the shape it has, and the one thing only the scroller can do.
  const published = useMemo(
    () => ({ filter: actions.filter, days, loading, jump }),
    [actions.filter, days, loading, jump],
  );
  useViewScope(published);

  // A reorder or a refilter makes every position mean a different photograph,
  // so the two things addressed by position have to let go of it: the scroll
  // offset, which would leave somebody halfway down a timeline they have not
  // seen the top of, and the selection, which would name forty photographs
  // nobody picked.
  const { view, setView } = useView();
  const looking = viewKey(view);
  const looked = useRef(looking);
  useEffect(() => {
    if (looked.current === looking) return;
    looked.current = looking;
    const el = scroller.current;
    if (el) el.scrollTop = 0;
    env.current.scrollTop = 0;
    env.current.anchor = null;
    exit();
  }, [looking, exit]);

  const press = useRef<Press | null>(null);
  const chase = useRef(0);
  /** Which tile a right-click landed on, primed before the menu opens. */
  const [menuAt, setMenuAt] = useState(-1);
  /**
   * Whether the menu is actually up.
   *
   * Apart from `menuAt`, which is set on the press *before* the menu opens and
   * stays set after it closes — it is the target, not the state. The armed
   * items need the state: a Delete left one click from firing has to go back to
   * sleep when the menu it is in goes away.
   */
  const [menuOpen, setMenuOpen] = useState(false);
  /**
   * The album about to be made, or null. Held here rather than inside the menu
   * because the menu is gone by the time the dialog is on screen — clicking
   * "Create album" closes it, which is what makes the dialog reachable at all.
   */
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);

  /** The tile a screen point falls in, read from the geometry rather than the DOM. */
  const indexAt = useCallback(
    (clientX: number, clientY: number): number => {
      const canvas = board.current;
      const e = env.current;
      if (!canvas || e.count === 0) return -1;
      const rect = canvas.getBoundingClientRect();
      const at = frameAt(e.levels, zoom.value);
      return itemAtPoint(e.days, e.count, at, clientX - rect.left, clientY - rect.top);
    },
    [zoom],
  );

  /**
   * The tile an event started on, from the element itself.
   *
   * Exact where the geometry is merely close: a press has to land on a tile to
   * mean anything, and the gaps between them and the day headings are not
   * tiles. Once a drag is running the geometry takes over, because by then the
   * pointer is allowed to be over a gap, over a heading, or off the screen
   * entirely and still be pointing at a row.
   */
  const tileAt = useCallback(
    (target: EventTarget | null): number => {
      const el = target instanceof Element ? target.closest("[data-tile]") : null;
      if (!(el instanceof HTMLElement)) return -1;
      const day = days[Number(el.dataset.day)];
      const offset = Number(el.dataset.tile);
      return day && Number.isFinite(offset) ? day.start + offset : -1;
    },
    [days],
  );

  /**
   * One frame of a running drag: scroll if the pointer is at an edge, then say
   * which tile it is over.
   *
   * Both have to happen every frame rather than on every pointer event, because
   * a pointer parked in the bottom band does not move at all while the grid
   * pours past underneath it.
   */
  const chaseFrame = useCallback(() => {
    const p = press.current;
    const el = scroller.current;
    if (!p?.dragging || !el) {
      chase.current = 0;
      return;
    }

    const rect = el.getBoundingClientRect();
    const above = p.y - rect.top;
    const below = rect.bottom - p.y;
    const speed = above < EDGE_BAND ? -edgeSpeed(above) : below < EDGE_BAND ? edgeSpeed(below) : 0;
    if (speed !== 0) {
      const limit = Math.max(0, el.scrollHeight - el.clientHeight);
      el.scrollTop = clamp(el.scrollTop + speed, 0, limit);
    }

    const index = indexAt(p.x, p.y);
    if (index >= 0) moveDrag(index);

    chase.current = requestAnimationFrame(chaseFrame);
  }, [indexAt, moveDrag]);

  const startDrag = useCallback(
    (p: Press) => {
      p.dragging = true;
      beginDrag(p.index);
      // So the drag keeps arriving here once the pointer has left the tile it
      // began on, which it does within about a tile's width.
      board.current?.setPointerCapture(p.id);
      if (!chase.current) chase.current = requestAnimationFrame(chaseFrame);
    },
    [beginDrag, chaseFrame],
  );

  const endPress = useCallback(() => {
    const p = press.current;
    if (!p) return;
    press.current = null;
    window.clearTimeout(p.timer);
    cancelAnimationFrame(chase.current);
    chase.current = 0;
    if (!p.dragging) return;
    const el = board.current;
    if (el?.hasPointerCapture(p.id)) el.releasePointerCapture(p.id);
    endDrag();
  }, [endDrag]);

  useEffect(() => () => cancelAnimationFrame(chase.current), []);

  const onPointerDown = useCallback(
    (ev: React.PointerEvent) => {
      // A right-click is not a press but a question about one tile, and the
      // menu that answers it opens on the event after this one. Setting the
      // target here rather than there is what lets the menu stay shut over a
      // gap in the grid: React has flushed this by the time `contextmenu`
      // arrives, so `disabled` is already right. Ctrl-click comes through here
      // too, because on a Mac that is the same question asked with one button.
      if (ev.button === 2 || (ev.button === 0 && ev.ctrlKey)) {
        setMenuAt(tileAt(ev.target));
        return;
      }
      if (ev.button !== 0 || ev.metaKey || ev.altKey) return;

      endPress();
      const index = tileAt(ev.target);
      if (index < 0) return;

      const touch = ev.pointerType === "touch";
      const p: Press = {
        id: ev.pointerId,
        index,
        fromX: ev.clientX,
        fromY: ev.clientY,
        x: ev.clientX,
        y: ev.clientY,
        touch,
        dragging: false,
        held: false,
        moved: false,
        timer: 0,
      };
      press.current = p;

      if (selecting) {
        // Whatever this press turns out to be, it is not the browser being
        // asked to select the page's text or drag a thumbnail off it.
        ev.preventDefault();
        if (ev.shiftKey) {
          span(index);
          return;
        }
        // A finger picks on release instead, so that the same touch can still
        // scroll the grid it is picking from.
        if (!touch) startDrag(p);
        return;
      }

      p.timer = window.setTimeout(() => {
        p.held = true;
        enter();
        if (p.touch) toggle(p.index);
        else startDrag(p);
      }, LONG_PRESS_MS);
    },
    [endPress, tileAt, selecting, span, startDrag, enter, toggle],
  );

  const onPointerMove = useCallback((ev: React.PointerEvent) => {
    const p = press.current;
    if (!p || ev.pointerId !== p.id) return;
    p.x = ev.clientX;
    p.y = ev.clientY;
    // A running drag needs nothing more from here: the frame loop reads the
    // coordinates this just wrote, so a pointer moving across forty tiles costs
    // one update rather than forty.
    if (p.dragging || p.moved) return;
    if (Math.abs(p.x - p.fromX) + Math.abs(p.y - p.fromY) <= PRESS_SLOP) return;

    // Wandered, so it was never a hold — a mouse on its way somewhere, or a
    // finger starting to scroll.
    p.moved = true;
    window.clearTimeout(p.timer);
    p.timer = 0;
  }, []);

  const onPointerUp = useCallback(
    (ev: React.PointerEvent) => {
      const p = press.current;
      if (!p || ev.pointerId !== p.id) return;
      // The tap half of the touch story: a finger that went down on a tile in
      // selection mode and came up without wandering picks it.
      if (!p.dragging && p.touch && !p.held && !p.moved && selecting) toggle(p.index);
      endPress();
    },
    [selecting, toggle, endPress],
  );

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      setSize((prev) =>
        prev.width === width && prev.height === height ? prev : { width, height },
      );
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // Scroll fires far more often than the screen refreshes, and every handler
  // here ends in a re-render, so coalesce to one per frame.
  const pending = useRef(0);
  const onScroll = useCallback(() => {
    if (pending.current) return;
    pending.current = requestAnimationFrame(() => {
      pending.current = 0;
      const el = scroller.current;
      if (!el) return;
      env.current.scrollTop = el.scrollTop;
      if (zoom.moving) applyFrame();
      else invalidate();
    });
  }, [applyFrame, zoom]);
  useEffect(() => () => cancelAnimationFrame(pending.current), []);

  // A ctrl-wheel gesture is one continuous travel rather than a series of
  // independent notches: the level is a function of how far the gesture has come
  // from where it began, so reaching a level costs the same 150px whether that
  // took one flick or twenty, and scrolling back the way you came unwinds it
  // exactly. This is what keeps a fast flick from spending its whole length
  // retargeting a transition that has not visibly started yet — the keyboard,
  // which cannot run away like that, still steps one level per press.
  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const gesture = { travel: 0, at: -Infinity, base: 0 };

    const onWheel = (ev: WheelEvent) => {
      if (!ev.ctrlKey && !ev.metaKey) return;
      // Also what stops the browser zooming the whole page underneath us.
      ev.preventDefault();

      if (ev.timeStamp - gesture.at > WHEEL_IDLE_MS) {
        gesture.travel = 0;
        gesture.base = Math.round(zoom.target);
      }
      gesture.at = ev.timeStamp;

      const unit =
        ev.deltaMode === 1 ? WHEEL_LINE_PX : ev.deltaMode === 2 ? el.clientHeight : 1;
      // Scrolling up zooms in, so travel runs against the wheel.
      gesture.travel -= ev.deltaY * unit;
      // Held to the ends of the scale, so pushing on past the largest tiles
      // never banks travel that has to be paid back before anything moves.
      gesture.travel = clamp(
        gesture.travel,
        -gesture.base * WHEEL_PER_LEVEL,
        (MAX_ZOOM - gesture.base) * WHEEL_PER_LEVEL,
      );

      const level = gesture.base + Math.trunc(gesture.travel / WHEEL_PER_LEVEL);
      if (level === Math.round(zoom.target)) return;

      anchorAt(ev.clientX, ev.clientY);
      zoom.to(level);
    };

    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [anchorAt, zoom]);

  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (!(ev.ctrlKey || ev.metaKey) || ev.altKey) return;
      const target = ev.target as HTMLElement | null;
      if (target?.isContentEditable) return;
      if (target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)) return;

      let act: (() => void) | null = null;
      if (ev.key === "+" || ev.key === "=") act = () => zoom.step(1);
      else if (ev.key === "-" || ev.key === "_") act = () => zoom.step(-1);
      else if (ev.key === "0") act = () => zoom.to(DEFAULT_ZOOM);
      if (!act) return;

      ev.preventDefault();
      anchorCentre();
      act();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [anchorCentre, zoom]);

  // Refreshed after every render, before the browser paints, so the frame loop
  // never places a tile that has just been unmounted.
  useLayoutEffect(() => {
    const e = env.current;
    e.days = days;
    e.levels = levels;
    e.count = total;
    e.height = size.height;
    e.box = box;

    const canvas = board.current;
    if (canvas) {
      const tiles: Slot[] = [];
      const heads: Slot[] = [];
      for (const child of canvas.children) {
        const el = child as HTMLElement;
        const day = Number(el.dataset.day);
        const offset = el.dataset.tile;
        if (offset === undefined) heads.push({ el, day, offset: 0 });
        else tiles.push({ el, day, offset: Number(offset) });
      }
      e.tiles = tiles;
      e.heads = heads;
    }
    updatePill();
  });

  // What the store is asked for, which is deliberately more than what is drawn.
  // Recomputed from the same geometry the renderer uses, so a fling lands on a
  // request for exactly the stretch of timeline under the viewport rather than
  // on a walk through everything above it.
  const wanted = visibleItems(
    days,
    total,
    frame,
    env.current.scrollTop,
    size.height,
    size.height * FETCH_OVERSCAN,
  );
  useEffect(() => {
    if (size.height === 0) return;
    request("grid", wanted.start, wanted.end);
  }, [request, wanted.start, wanted.end, size.height]);

  const visible = useMemo(() => {
    const out: number[] = [];
    for (let index = range.start; index < range.end; index++) out.push(index);
    return out;
  }, [range.start, range.end]);

  // A Live Photo's motion is queued separately from its thumbnail, so a tile
  // can be drawn and still be waiting to come to life. Placeholders are not in
  // here: there is nothing to ask about an asset we have not been sent yet.
  const pendingKey = useMemo(() => {
    const ids: string[] = [];
    for (const index of visible) {
      const item = itemAt(index);
      if (item && (item.state === "pending" || item.live === "pending")) ids.push(item.id);
    }
    return ids.join(",");
    // `timeline` is a dependency because `itemAt` reads through a ref, and the
    // store's identity is the only thing that changes when a page lands.
  }, [visible, itemAt, timeline]);

  // Only tiles actually on screen are polled. During a backfill the library can
  // be tens of thousands of pending items, and asking about all of them every
  // few seconds would cost more than generating the thumbnails.
  useEffect(() => {
    if (!pendingKey) return;
    const ids = pendingKey.split(",");
    const controller = new AbortController();

    const id = setInterval(() => {
      fetchStates(ids, controller.signal)
        .then(patch)
        .catch(() => {
          // A dropped poll is not worth surfacing; the next tick retries.
        });
    }, POLL_MS);

    return () => {
      controller.abort();
      clearInterval(id);
    };
  }, [pendingKey, patch]);

  const headings = headingsFor(days, range);

  const onMenu = menuAt >= 0 ? itemAt(menuAt) : undefined;

  /**
   * What the menu's actions are about.
   *
   * A right-click inside a live selection means the selection, which is what
   * every file manager and every photo app does and what makes "select forty,
   * right-click, delete" one gesture. A right-click anywhere else means that one
   * tile, and says so — including whether it is a photo or a video, which the
   * grid knows and the toast otherwise would not.
   */
  const onSelection = selecting && menuAt >= 0 && selected(menuAt);
  const menuTarget = useMemo(
    () => (onSelection ? { ranges } : { ids: onMenu ? [onMenu.id] : [] }),
    [onSelection, ranges, onMenu],
  );
  // The noun is always the tile under the pointer's, even for a selection: a
  // selection of one is still a photo or a video, and describeAction only
  // reaches for the noun when the count is one.
  const menuNoun = nounFor(onMenu?.kind ?? "image");
  const menuCount = onSelection ? picked : 1;
  const describe = (verb: string) =>
    describeAction(verb, { kind: "items", count: menuCount, noun: menuNoun });

  // A right-click on a square whose photograph has not been fetched yet names
  // nothing: the position is real, but the menu would be acting on an id it does
  // not have. Selecting it first is the way to reach it.
  const nothingToAct = !onSelection && !onMenu;

  const act = useCallback(
    (run: (target: typeof menuTarget, noun?: ReturnType<typeof nounFor>) => Promise<void>) => {
      void run(menuTarget, menuNoun);
      // The indices this menu was about are about to mean other photographs.
      if (onSelection) exit();
    },
    [menuTarget, menuNoun, onSelection, exit],
  );

  // Hiding is not armed, and deleting is.
  //
  // The two clicks a delete costs are buying the one thing that cannot be
  // bought back — a purge, eventually, of the only copy. Hiding takes nothing
  // away: the photograph is still archived, still verified, still restorable in
  // two clicks from a page that is one tap from here, and the toast that
  // follows carries an Undo. Making it as hard to do as a delete would say the
  // two are equally serious, which would be a lie in the direction that makes
  // people stop reading the word "Confirm".
  const fileAway = useCallback(
    (bucket: Bucket) => {
      void actions.hide?.(bucket, menuTarget, menuNoun);
      if (onSelection) exit();
    },
    [actions, menuTarget, menuNoun, onSelection, exit],
  );

  // Which photograph the ticks in "Add to album" are about, when they are about
  // one. A selection of forty has forty answers and no useful way to draw them;
  // a selection of one is still one photograph, and the tile under the pointer
  // is it. See AlbumPicker.
  const single = onSelection ? (picked === 1 ? (onMenu?.id ?? null) : null) : (onMenu?.id ?? null);

  const fileInto = useCallback(
    (album: AlbumRef) => {
      void actions.file?.(album, menuTarget, menuNoun);
      if (onSelection) exit();
    },
    [actions, menuTarget, menuNoun, onSelection, exit],
  );

  const unfileFrom = useCallback(
    (album: AlbumRef) => {
      void actions.unfile?.(album, menuTarget, menuNoun);
      if (onSelection) exit();
    },
    [actions, menuTarget, menuNoun, onSelection, exit],
  );

  // The selection is captured here rather than read when the dialog is
  // submitted: by then the menu is gone, and a selection somebody has since
  // changed would put the wrong photographs into the new album.
  const startCreate = useCallback(
    (name: string) => {
      setCreating({
        name,
        bucket: actions.bucket,
        target: { ...menuTarget, filter: actions.filter, view: actions.view },
      });
    },
    [actions.bucket, actions.filter, actions.view, menuTarget],
  );

  const created = useCallback(
    (album: CreatedAlbum) => {
      const added = album.added ?? 0;
      toast.add({
        type: "success",
        title: `“${album.title}” created`,
        description: added > 0 ? `${counted(added, menuNoun)} in it.` : "It starts empty.",
      });
      if (onSelection) exit();
    },
    [menuNoun, onSelection, exit],
  );

  // Taking a selection out of the album it is being browsed in.
  //
  // Only drawn there: everywhere else — the library, a person, a category —
  // there is no album for "remove" to be about, and an item that acted on the
  // last album somebody happened to visit would be a menu you cannot read.
  //
  // Armed, like the delete below it, because "remove these forty" is a thing to
  // have meant rather than a thing to discover. Not red, unlike the delete,
  // because nothing is destroyed: every photograph stays in the library and in
  // its other albums, and the toast carries an Undo whenever it can.
  const albumHere = actions.album;
  const removeFromAlbum = albumHere ? (
    <ArmedMenuItem
      label={`${describe("Remove")} from album`}
      icon={<ListMinus />}
      tone="neutral"
      onConfirm={() => unfileFrom(albumHere)}
      open={menuOpen}
      disabled={nothingToAct}
    />
  ) : null;

  return (
    <>
      {/* One menu for the whole grid rather than one per tile: a screenful at the
          smallest zoom is a couple of thousand squares, and that many menus would
          cost more to keep mounted than the thumbnails in them. Which tile it is
          about is settled by the press that opens it. */}
      <ContextMenu disabled={menuAt < 0} onOpenChange={setMenuOpen}>
        <div className="relative min-h-0 flex-1">
          {/* Left empty for React: the frame loop owns this element's text and
              opacity, and reparenting a node React thinks it manages would leave
              the two writing to different places. */}
          <div
            ref={pill}
            className="pointer-events-none absolute top-2.5 left-[22px] z-[5] rounded-full border bg-card/[0.88] px-3 py-[5px] text-xs font-semibold opacity-0 backdrop-blur-[8px] transition-opacity duration-150"
          />

          <div
            // The padding is what the floating bar stands on: without it the last
            // row of the library can only be reached by scrolling it behind the tabs.
            className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-3 pb-24"
            ref={scroller}
            onScroll={onScroll}
          >
            {/* Rows are placed by transform rather than `top`: the browser can skip
                layout entirely when only a transform changes, which is what makes
                both scrolling and zooming cost a composite instead of a reflow. */}
            {/* `contents` so the trigger is a listener rather than a box: the board
                is the element the frame loop places tiles in, and putting a second
                one around it would change what they are positioned against. */}
            <ContextMenuTrigger className="contents">
              <div
                ref={board}
                className={cn("relative w-full", selecting && "select-none")}
                style={{ height: frame.totalHeight }}
                onPointerDown={onPointerDown}
                onPointerMove={onPointerMove}
                onPointerUp={onPointerUp}
                onPointerCancel={endPress}
                onLostPointerCapture={endPress}
                // Belt and braces for the keyboard's own menu key, which opens the
                // menu without a press to have primed it.
                onContextMenu={(ev) => setMenuAt(tileAt(ev.target))}
              >
                {headings.map((d) => (
                  <div
                    key={days[d].id}
                    data-day={d}
                    className="absolute top-0 left-0 flex w-full items-end gap-2.5 pb-2.5"
                    style={{
                      height: frame.headerHeight,
                      transform: `translateY(${headerY(frame, d)}px)`,
                    }}
                  >
                    <h2 className="text-[15px] font-semibold">{days[d].label}</h2>
                    <span className="text-xs text-faint">{days[d].count}</span>
                  </div>
                ))}

                {/* Every square in range is drawn, whether or not there is a photo
                    for it yet. The two cases share one geometry — the index decides
                    where the square is, and only what goes inside it depends on
                    having been fetched. */}
                {visible.map((index) => {
                  const day = dayIndexOf(days, index);
                  const offset = index - days[day].start;
                  const r = tileRect(frame, day, offset);
                  const transform = `translate(${r.x}px,${r.y}px) scale(${r.size / box})`;
                  const item = itemAt(index);
                  // The index is the address, so a square waiting on its photograph
                  // is selected on exactly the same terms as one holding it.
                  const on = selecting && selected(index);
                  return item ? (
                    <Tile
                      key={item.id}
                      item={item}
                      size={box}
                      thumbSize={thumb}
                      day={day}
                      offset={offset}
                      transform={transform}
                      selecting={selecting}
                      selected={on}
                      onOpen={onOpen}
                    />
                  ) : (
                    <Skeleton
                      key={`@${index}`}
                      size={box}
                      day={day}
                      offset={offset}
                      transform={transform}
                      selecting={selecting}
                      selected={on}
                    />
                  );
                })}
              </div>
            </ContextMenuTrigger>

            {error ? (
              <div className={cn(NOTICE, "text-destructive")}>
                <span>{error}</span>
                <Button type="button" variant="outline" size="sm" onClick={retry}>
                  Retry
                </Button>
              </div>
            ) : null}

            {/* No "loading" notice any more: the grid is already the right size
                and full of the squares the photos are going to land in, which says
                it better than a line of text at the bottom could.

                An empty grid with a filter on it is a different statement from an
                empty archive, and saying the wrong one sends somebody looking for
                photographs that are exactly where they left them. */}
            {ready && total === 0 && !error ? (
              <div className={NOTICE}>
                {isFiltered(view) ? (
                  <>
                    <span>Nothing here matches these filters.</span>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => setView({ sort: view.sort })}
                    >
                      Clear filters
                    </Button>
                  </>
                ) : (
                  <span>Nothing here yet. Run a backup from the phone.</span>
                )}
              </div>
            ) : null}
          </div>

          {mounted ? <ZoomSlider zoom={zoom} /> : null}
        </div>

        <ContextMenuContent>
          <ContextMenuItem
            onClick={() => {
              if (menuAt < 0) return;
              enter();
              toggle(menuAt);
            }}
          >
            <SquareCheck />
            {onSelection ? "Deselect" : "Select"}{" "}
            {onMenu ? (onMenu.kind === "video" ? "Video" : "Photo") : "Item"}
          </ContextMenuItem>

          <ContextMenuSeparator />

          {actions.scope === "trash" ? (
            <>
              <ContextMenuItem
                disabled={nothingToAct}
                onClick={() => act(actions.restore)}
              >
                <RotateCcw />
                {describe("Restore")}
              </ContextMenuItem>
              {/* Armed rather than confirmed in a dialog: two clicks in the place
                  the pointer already is, instead of a modal over the grid. */}
              <ArmedMenuItem
                label={describe("Delete")}
                confirm="Confirm — no undo"
                icon={<Trash2 />}
                onConfirm={() => act(actions.purge)}
                open={menuOpen}
                disabled={nothingToAct}
              />
            </>
          ) : actions.scope === "vault" ? (
            // No delete, and deliberately. Taking a photograph out of the vault
            // and then deleting it is two decisions, and a button that decrypted
            // a file in order to throw it away would be spending the password on
            // the one operation that does not need it.
            //
            // Filing is here all the same: a hidden photograph belongs in hidden
            // albums, and the alternative would be taking it out of the vault to
            // organise it and putting it back.
            <>
              <AddToAlbumSub
                bucket={actions.bucket}
                assetId={single}
                onAdd={fileInto}
                onRemove={unfileFrom}
                onCreate={startCreate}
                disabled={nothingToAct}
              />
              <ContextMenuItem disabled={nothingToAct} onClick={() => act(actions.restore)}>
                <RotateCcw />
                {describe(actions.bucket === "hidden" ? "Unhide" : "Unarchive")}
              </ContextMenuItem>
              {removeFromAlbum}
            </>
          ) : (
            <>
              <AddToAlbumSub
                assetId={single}
                onAdd={fileInto}
                onRemove={unfileFrom}
                onCreate={startCreate}
                disabled={nothingToAct}
              />
              <ContextMenuItem disabled={nothingToAct} onClick={() => fileAway("archive")}>
                <Archive />
                {describe("Archive")}
              </ContextMenuItem>
              <ContextMenuItem disabled={nothingToAct} onClick={() => fileAway("hidden")}>
                <EyeOff />
                {describe("Hide")}
              </ContextMenuItem>
              {removeFromAlbum}

              <ContextMenuSeparator />

              <ArmedMenuItem
                label={describe("Delete")}
                icon={<Trash2 />}
                onConfirm={() => act(actions.remove)}
                open={menuOpen}
                disabled={nothingToAct}
              />
            </>
          )}
        </ContextMenuContent>
      </ContextMenu>

      <CreateAlbumDialog
        request={creating}
        onClose={() => setCreating(null)}
        onCreated={created}
      />
    </>
  );
}

/** The largest level the running transition still has to draw. */
function boxLevelFor(zoom: Zoom): number {
  return Math.min(MAX_ZOOM, Math.ceil(Math.max(zoom.value, zoom.target)));
}

/** Every heading that could be on screen: the visible days, plus the next one down. */
function headingsFor(days: Day[], range: ItemRange): number[] {
  if (days.length === 0 || range.end <= range.start) return [];
  const first = dayIndexOf(days, range.start);
  const last = Math.min(dayIndexOf(days, range.end - 1) + 1, days.length - 1);
  const out: number[] = [];
  for (let d = first; d <= last; d++) out.push(d);
  return out;
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v;
}

/**
 * How fast to scroll for a pointer `away` pixels from the edge of the grid.
 *
 * Squared rather than linear, so most of the band is a crawl and the last few
 * pixels are the fast one. A linear ramp makes the whole band feel like it is
 * running away from the pointer; this way the speed is something the hand
 * chooses rather than something it endures.
 */
function edgeSpeed(away: number): number {
  const t = 1 - clamp(away, 0, EDGE_BAND) / EDGE_BAND;
  return EDGE_SPEED * t * t;
}
