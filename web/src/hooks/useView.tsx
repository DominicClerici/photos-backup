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

import type { TimelineFilter, View } from "@/lib/api";
import { DEFAULT_VIEW, facetsFor, viewKey, withinFacets, type Facets } from "@/lib/view";
import type { Day } from "@/lib/layout";

/**
 * What a grid says about itself so that the floating control can act on it.
 *
 * The same problem the selection has, solved the same way: the control is
 * mounted by the root layout and the grid by whichever page is browsing, so
 * neither can reach the other and this is where they meet. See SelectionActions.
 */
export interface GridView {
  /**
   * Which collection this grid is of, which decides what there is left to
   * filter by — see facetsFor. Undefined is the library.
   */
  filter?: TimelineFilter;
  /**
   * Every heading the grid will draw, with its size and its position. What the
   * calendar's range comes from, and what a date is turned into a position with.
   */
  days: Day[];
  /**
   * Whether the day table in hand is being replaced. True from the moment a
   * different view is asked for until its table lands, which is exactly the
   * window in which the days above describe a timeline nobody is looking at.
   */
  loading: boolean;
  /** Puts a position at the top of the viewport. What jumping to a date is. */
  jump: (index: number) => void;
}

export interface ViewState {
  /** How the grid on screen is being looked at: the order and the narrowing. */
  view: View;
  setView: (view: View) => void;
  /** Which filters this grid has left to offer. See facetsFor. */
  facets: Facets;
  /** What the grid published, or null when there is no grid on screen. */
  grid: GridView | null;
  /** Called by a grid to own the view for as long as it is mounted. */
  register: () => () => void;
  /** Called by the same grid whenever what it has to say changes. */
  provide: (grid: GridView) => void;
}

const Context = createContext<ViewState | null>(null);

/**
 * The sort and the filters, held above both the grid and the floating bar.
 *
 * It lives here for the reason the selection does — the bar is mounted by the
 * root layout and the grid by a page — and it resets for a reason of its own.
 * A view is of one timeline: "the videos in this album, longest first" is not a
 * thing to still be looking at after walking into a different album, and a
 * filter silently carried through a navigation is one somebody has to go and
 * find before their photographs come back. So leaving the grid is leaving the
 * view, exactly as it is leaving the selection.
 */
export function ViewProvider({ children }: { children: ReactNode }) {
  const [view, setView] = useState<View>(DEFAULT_VIEW);
  const [grid, setGrid] = useState<GridView | null>(null);

  const register = useCallback(() => {
    return () => {
      setGrid(null);
      setView(DEFAULT_VIEW);
    };
  }, []);

  const provide = useCallback((next: GridView) => setGrid(next), []);

  const facets = useMemo(() => facetsFor(grid?.filter), [grid?.filter]);

  // A facet the grid on screen cannot offer is one nobody could turn off, so it
  // is corrected here rather than left to every caller of setView. Which grid is
  // on screen can change under a view — a link from the library into the Videos
  // category — and this is the only place that sees both.
  const held = useMemo(() => withinFacets(view, facets), [view, facets]);
  const settled = viewKey(held) !== viewKey(view);
  useEffect(() => {
    if (settled) setView(held);
  }, [settled, held]);

  const value = useMemo<ViewState>(
    () => ({ view: held, setView, facets, grid, register, provide }),
    [held, facets, grid, register, provide],
  );

  return <Context.Provider value={value}>{children}</Context.Provider>;
}

export function useView(): ViewState {
  const held = useContext(Context);
  if (!held) throw new Error("useView must be used inside a ViewProvider");
  return held;
}

/**
 * Claims the view for a grid, for as long as that grid is on screen.
 *
 * What makes the sort-and-filter pill appear on the pages that have a gallery
 * and nowhere else, and what drops a view when one is navigated away from.
 *
 * Null is a grid that does not claim it, which is the search results: the order
 * there is the ranking and the filter is the query, so a pill offering "Newest"
 * over a relevance ranking would be a control that either does nothing or
 * throws the answer away. Its chips are the filter, and they are on the page.
 */
export function useViewScope(grid: GridView | null): void {
  const { register, provide } = useView();
  // Whether, not which. Registering is what a grid does once on the way in and
  // undoes on the way out, and its undoing drops the view — so it must not be
  // re-run every time the grid has something new to say about itself.
  const claims = grid !== null;

  useEffect(() => {
    if (!claims) return;
    return register();
  }, [register, claims]);

  useEffect(() => {
    if (grid) provide(grid);
  }, [provide, grid]);
}
