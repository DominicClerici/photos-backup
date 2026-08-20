"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { apply, between, count, has, NONE, type Ranges, type SelectMode } from "@/lib/ranges";
import type { Bucket, Target, TimelineFilter, View } from "@/lib/api";
import type { Noun } from "@/lib/format";

/**
 * The run a drag is currently laying down, kept apart from what it will commit.
 *
 * This is the whole of how a drag undoes itself. The preview is always the run
 * between the tile the drag began on and the tile under the pointer *now*, so
 * dragging back down over five rows just makes that run shorter — nothing has
 * to remember which tiles the gesture touched on its way out, and a tile that
 * was selected before the drag started is still selected when the drag has
 * shrunk back past it.
 */
interface Drag {
  from: number;
  to: number;
  mode: SelectMode;
}

/**
 * An album, as much of one as filing it needs to know.
 *
 * The title travels with the id because the toast says it, and by the time the
 * request has landed the menu that knew the name is gone. It is optional
 * because one caller genuinely does not have it: an album page whose heading is
 * still loading knows which album it is and not yet what it is called.
 */
export interface AlbumRef {
  id: string;
  title?: string;
}

/**
 * What can be done to a selection, published by whichever grid owns it.
 *
 * The grid and the floating control are mounted by different things — the
 * control by the root layout, the grid by whichever page is browsing — so the
 * control cannot reach the timeline it is reporting on. This is how it does:
 * the grid hands its actions up when it mounts, and the pill spends them.
 *
 * Which actions are offered is the scope's to decide, not the pill's. There are
 * three grids and each one means something different by the same gesture: in
 * the library a selection can be deleted, hidden or archived; in the trash it
 * can be restored or destroyed; in the vault it can only come back out.
 *
 * That last one is deliberate rather than unfinished. There is no delete inside
 * the vault: taking a photograph out and then deleting it is two decisions, and
 * a single button that decrypted a file in order to throw it away would be
 * spending the password on the one operation that does not need it.
 */
export interface SelectionActions {
  scope: "library" | "trash" | "vault";
  /**
   * Which bucket, when the scope is the vault. What "Unarchive" is named from,
   * and which half of the archive the "Add to album" menu lists.
   */
  bucket?: Bucket;
  /**
   * The album this grid *is*, when it is one.
   *
   * What makes "Remove from album" appear and what it is about. A grid that is
   * a person, a category or the whole library is not in any album, so there is
   * nothing to take a selection out of and the item is not drawn.
   */
  album?: AlbumRef;
  /**
   * Which timeline the positions in a selection are counted in.
   *
   * Every action below already closes over it, so nothing that calls one needs
   * this. It is here for the surfaces that build a request of their own — the
   * create-album dialog is the only one — because a range without the filter it
   * was counted in names a different set of photographs.
   */
  filter?: TimelineFilter;
  /**
   * How that timeline is being sorted and filtered, for the same surfaces and
   * the same reason. A range counted in a grid showing only the videos names
   * other photographs anywhere else.
   */
  view?: View;
  /**
   * A target this grid's own actions would accept, from one addressed by
   * position. Undefined where the two are the same thing, which is everywhere
   * but the search results.
   *
   * The actions below already do this to whatever they are handed, so nothing
   * that calls one needs it. It is here for the one surface that builds a
   * request of its own — the create-album dialog, which sends a target to an
   * endpoint none of these actions owns — and it is the same reason `filter`
   * and `view` are here: a range without the whole description of the grid it
   * was counted in names other photographs. A ranking has no such description,
   * so its ranges are spelled out into ids instead. See useSearchActions.
   */
  resolve?: (target: Target) => Promise<Target>;
  /** Library only: to Recently Deleted, undoably. */
  remove: (target: Target, noun?: Noun) => Promise<void>;
  /**
   * Library only: into the Archive or the Hidden bucket.
   *
   * Needs no password — see the vault package — which is what lets this sit in
   * a right-click menu beside Delete rather than behind a dialog.
   */
  hide: (bucket: Bucket, target: Target, noun?: Noun) => Promise<void>;
  /** Trash and vault: back into the library, where it was. */
  restore: (target: Target, noun?: Noun) => Promise<void>;
  /** Trash only: gone, with no undo. */
  purge: (target: Target, noun?: Noun) => Promise<void>;
  /**
   * Library and vault: into an album, on the same side of the lock.
   *
   * The album travels whole rather than by id because the toast says its name,
   * and by the time the request has landed the menu that knew the name is gone.
   */
  file: (album: AlbumRef, target: Target, noun?: Noun) => Promise<void>;
  /** The same, backwards. Removes the grouping and nothing else. */
  unfile: (album: AlbumRef, target: Target, noun?: Noun) => Promise<void>;
}

interface Model {
  active: boolean;
  ranges: Ranges;
  drag: Drag | null;
  /** The last tile deliberately picked; what a shift-click measures from. */
  anchor: number;
}

const EMPTY: Model = { active: false, ranges: NONE, drag: null, anchor: -1 };

export interface SelectionState {
  /** Whether the grid is in selection mode. */
  active: boolean;
  /** How many items are selected, counting whatever a running drag would add. */
  count: number;
  /**
   * The selection itself, as runs of timeline indices.
   *
   * What the actions this is groundwork for will be handed. Deliberately not
   * ids: a selection can cover a hundred thousand photographs the client has
   * never fetched, and the index is the only name every one of them has.
   */
  ranges: Ranges;
  selected: (index: number) => boolean;
  /** Whether a grid is on screen for the floating control to belong to. */
  grid: boolean;
  /**
   * What the grid on screen can do to the selection, or null before one has
   * said. Null is also what the pill draws for: an action that has not arrived
   * yet is one it must not offer.
   */
  actions: SelectionActions | null;
  /** Whether the action sheet above the pill is open. */
  sheet: boolean;
  setSheet: (open: boolean) => void;
  /** Turns selection mode on, selecting nothing. */
  enter: () => void;
  /** Turns it off and drops the selection. */
  exit: () => void;
  /** Picks or unpicks one tile, and moves the anchor to it. */
  toggle: (index: number) => void;
  /** Shift-click: the run from the anchor to here, in the tile's own direction. */
  span: (index: number) => void;
  beginDrag: (index: number) => void;
  moveDrag: (index: number) => void;
  endDrag: () => void;
  /** Called by a grid to own the selection for as long as it is mounted. */
  register: () => () => void;
  /**
   * Called by the same grid to say what can be done to it.
   *
   * Apart from `register` because it happens on a different schedule: the
   * registration is a mount and an unmount, and dropping the selection is the
   * right thing to do on both. The actions change whenever the timeline they
   * close over does, and a selection must survive that.
   */
  provide: (actions: SelectionActions) => void;
}

const Context = createContext<SelectionState | null>(null);

/**
 * Selection state, held above both the grid and the floating bar.
 *
 * They are mounted by different things — the bar by the root layout, the grid
 * by whichever page is browsing — and the pill has to say how many items the
 * grid has selected, so the state is the one place the two can meet. It lives
 * here rather than in the grid for the same reason the tab bar does: switching
 * sections must not blink it out of existence mid-navigation.
 */
export function SelectionProvider({ children }: { children: ReactNode }) {
  const [model, setModel] = useState<Model>(EMPTY);
  const [sheet, setSheet] = useState(false);
  const [grids, setGrids] = useState(0);
  const [actions, setActions] = useState<SelectionActions | null>(null);

  const enter = useCallback(() => {
    setModel((m) => (m.active ? m : { ...m, active: true }));
  }, []);

  const exit = useCallback(() => {
    setModel(EMPTY);
    setSheet(false);
  }, []);

  const toggle = useCallback((index: number) => {
    setModel((m) => {
      const mode: SelectMode = has(m.ranges, index) ? "remove" : "add";
      return {
        ...m,
        ranges: apply(m.ranges, index, index + 1, mode),
        anchor: index,
      };
    });
  }, []);

  const span = useCallback((index: number) => {
    setModel((m) => {
      // With nothing to measure from — a shift-click as the very first thing
      // done in selection mode — the run is the one tile.
      const from = m.anchor < 0 ? index : m.anchor;
      const mode: SelectMode = has(m.ranges, index) ? "remove" : "add";
      const run = between(from, index);
      return {
        ...m,
        ranges: apply(m.ranges, run.start, run.end, mode),
        anchor: index,
      };
    });
  }, []);

  const beginDrag = useCallback((index: number) => {
    setModel((m) => ({
      ...m,
      // The tile under the press decides the whole gesture: begun on a selected
      // photo, the drag takes selection away everywhere it goes.
      drag: { from: index, to: index, mode: has(m.ranges, index) ? "remove" : "add" },
      anchor: index,
    }));
  }, []);

  const moveDrag = useCallback((index: number) => {
    setModel((m) =>
      !m.drag || m.drag.to === index ? m : { ...m, drag: { ...m.drag, to: index } },
    );
  }, []);

  const endDrag = useCallback(() => {
    setModel((m) => (m.drag ? { ...m, ranges: settle(m), drag: null } : m));
  }, []);

  const register = useCallback(() => {
    setGrids((n) => n + 1);
    return () => {
      setGrids((n) => n - 1);
      // A selection is of a particular timeline, and an index means something
      // else in the next one. Leaving the grid is leaving the selection.
      setModel(EMPTY);
      setSheet(false);
      // And an action closes over that same timeline, so it must not outlive
      // the grid that offered it.
      setActions(null);
    };
  }, []);

  const provide = useCallback((next: SelectionActions) => setActions(next), []);

  // What the grid draws: the committed runs with the running drag laid over
  // them. Recomputed rather than stored, so there is one selection rather than
  // one and a shadow of it that has to be kept in step.
  const shown = useMemo(() => settle(model), [model]);

  useEffect(() => {
    if (!model.active) return;
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key !== "Escape") return;
      // Anything over the grid gets the key to itself. Escape in a dialog or a
      // menu means "never mind, close this" — throwing the selection away as
      // well would undo the very thing somebody was being asked about, and
      // dismissing the album menu is not a reason to lose forty photographs.
      if (
        document.querySelector(
          "[role=alertdialog],[data-slot=dialog-content],[data-slot=context-menu-content],[data-slot=dropdown-menu-content],[data-slot=popover-content]",
        )
      ) {
        return;
      }
      ev.preventDefault();
      exit();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [model.active, exit]);

  const value = useMemo<SelectionState>(
    () => ({
      active: model.active,
      ranges: shown,
      count: count(shown),
      selected: (index: number) => has(shown, index),
      grid: grids > 0,
      actions,
      sheet: sheet && model.active,
      setSheet,
      enter,
      exit,
      toggle,
      span,
      beginDrag,
      moveDrag,
      endDrag,
      register,
      provide,
    }),
    [
      model.active,
      shown,
      grids,
      actions,
      sheet,
      enter,
      exit,
      toggle,
      span,
      beginDrag,
      moveDrag,
      endDrag,
      register,
      provide,
    ],
  );

  return <Context.Provider value={value}>{children}</Context.Provider>;
}

export function useSelection(): SelectionState {
  const held = useContext(Context);
  if (!held) throw new Error("useSelection must be used inside a SelectionProvider");
  return held;
}

/**
 * Claims the selection for a grid, for as long as that grid is on screen.
 *
 * What makes the floating control appear on the pages that have a gallery and
 * nowhere else, and what drops a selection when one is navigated away from.
 */
export function useSelectionScope(actions: SelectionActions): void {
  const { register, provide } = useSelection();
  useEffect(register, [register]);
  useEffect(() => provide(actions), [provide, actions]);
}

function settle(model: Model): Ranges {
  const { ranges, drag } = model;
  if (!drag) return ranges;
  const run = between(drag.from, drag.to);
  return apply(ranges, run.start, run.end, drag.mode);
}
