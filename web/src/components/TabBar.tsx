"use client";

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { Activity, Ellipsis, Images, Library, Search } from "lucide-react";

import { openPalette, usePaletteOpen } from "@/hooks/usePalette";
import { cn } from "@/lib/utils";
import { FilterPill } from "./FilterPill";
import { SelectionPill } from "./SelectionPill";

/** Every tab is a place. Search is not a tab; see SearchButton. */
const TABS: { href: string; label: string; Icon: typeof Images }[] = [
  { href: "/", label: "Gallery", Icon: Images },
  { href: "/collections", label: "Collections", Icon: Library },
  { href: "/status", label: "Status", Icon: Activity },
  { href: "/other", label: "Other", Icon: Ellipsis },
];

/** Where the highlight sits, in pixels along the row. */
interface Pill {
  x: number;
  w: number;
}

/**
 * The section switcher: a floating bar over whatever page is on screen.
 *
 * The tabs themselves never change size — only their colour does, and the
 * highlight behind them is a single element that slides and stretches from one
 * to the next. Anything that changed a tab's own width (a bolder weight on the
 * active one, a label that appears only when selected) would have the row
 * re-flowing underneath the very animation meant to track it.
 */
export function TabBar() {
  const pathname = usePathname();
  const row = useRef<HTMLUListElement>(null);
  const tabs = useRef<(HTMLElement | null)[]>([]);

  const [pill, setPill] = useState<Pill | null>(null);
  // The first placement is where the highlight *starts*, so it has to be drawn
  // there rather than travelled to: otherwise every page load flies it in from
  // the left edge.
  const [placed, setPlaced] = useState(false);

  const active = TABS.findIndex(
    (tab) => pathname === tab.href || (tab.href !== "/" && pathname.startsWith(`${tab.href}/`)),
  );

  const measure = useCallback(() => {
    const el = tabs.current[active];
    // offsetLeft is relative to the row, which is the nearest positioned
    // ancestor and also what the highlight is absolutely placed against.
    setPill(el ? { x: el.offsetLeft, w: el.offsetWidth } : null);
  }, [active]);

  useLayoutEffect(measure, [measure]);

  // Labels drop out below 640px and the row shrinks with them, so the highlight
  // is measured from the live geometry rather than assumed.
  useEffect(() => {
    const el = row.current;
    if (!el) return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [measure]);

  useEffect(() => {
    if (!pill || placed) return;
    const id = requestAnimationFrame(() => setPlaced(true));
    return () => cancelAnimationFrame(id);
  }, [pill, placed]);

  return (
    <nav
      // The viewer is a full-screen overlay above this bar. It already hides it,
      // but a control nobody can see should not still be reachable by Tab —
      // hence the visibility, and the fade that comes with it. See globals.css.
      data-slot="tab-bar"
      aria-label="Sections"
      className="pointer-events-none fixed inset-x-0 bottom-5 z-30 flex justify-center px-3 transition-[opacity,visibility] duration-200 ease-out"
    >
      {/* The tabs and everything moored to them. The wrapper is only here to
          give the floating controls an edge to hang off: they are positioned
          against the row's left and right edges and grow away from them, so a
          selection of eleven thousand photographs widens its pill without
          nudging a tab. */}
      <div className="pointer-events-none relative flex max-w-full">
        {/* Anchored by its right edge and unconstrained on the left, so the row
            grows leftwards as the pills in it find things to say. Both of them
            are things done *to* a grid, and neither is drawn without one.

            `w-max` is load-bearing. An absolutely positioned box with only
            `right` set is shrink-to-fit against the space between that edge and
            the containing block's — which at `right-full` is exactly none, so
            every label inside would collapse to nothing while the pills kept
            their padding and looked merely empty. */}
        <div className="absolute right-full bottom-0 mr-2 flex w-max items-end gap-2">
          <FilterPill />
          <SelectionPill />
        </div>

        <ul
          ref={row}
          className="pointer-events-auto relative flex max-w-full items-center gap-1 rounded-full border bg-card/80 p-1.5 shadow-lg backdrop-blur-xl"
        >
          {pill ? (
            <span
              aria-hidden="true"
              className={cn(
                "absolute top-1.5 bottom-1.5 left-0 rounded-full bg-foreground/[0.09]",
                // Overshoot-free ease-out: it leaves fast and settles slowly, which
                // is what makes a short 250px slide read as deliberate.
                placed &&
                  "transition-[transform,width] duration-300 ease-[cubic-bezier(0.22,1,0.36,1)] motion-reduce:transition-none",
              )}
              style={{ width: pill.w, transform: `translateX(${pill.x}px)` }}
            />
          ) : null}

          {TABS.map(({ href, label, Icon }, i) => {
            const current = i === active;

            return (
              <li key={href}>
                <Link
                  href={href}
                  ref={(el) => {
                    tabs.current[i] = el;
                  }}
                  aria-current={current ? "page" : undefined}
                  className={cn(
                    "relative flex h-10 items-center gap-2 rounded-full px-3.5 text-[13px] font-medium tracking-[0.01em] transition-colors duration-200 focus-visible:ring-2 focus-visible:ring-ring/70 focus-visible:outline-none sm:px-4",
                    current
                      ? "text-foreground"
                      : "text-muted-foreground hover:bg-foreground/[0.05] hover:text-foreground",
                  )}
                >
                  <Icon
                    className={cn(
                      "size-[18px] shrink-0 transition-colors duration-200",
                      current && "text-primary",
                    )}
                    aria-hidden="true"
                  />
                  {/* Below 640px the row is icons only. The label stays in the
                      accessibility tree, so each tab keeps its name. */}
                  <span className="max-sm:sr-only">{label}</span>
                </Link>
              </li>
            );
          })}
        </ul>

        {/* The mirror of the selection pill, on the other side of the row. */}
        <div className="absolute bottom-0 left-full ml-2 flex w-max items-end">
          <SearchButton />
        </div>
      </div>
    </nav>
  );
}

/**
 * Search, standing to the right of the tab bar.
 *
 * It is a button and not a tab because a search is a question rather than a
 * destination: there is nothing on /search until something has been asked, and
 * sending somebody to an empty results page to type into it would be one extra
 * screen between the thought and the answer. Sitting it outside the row says as
 * much before it is pressed — the tabs are four places, and this is the thing
 * you do to them.
 *
 * The route still exists and is still what the button lights up for: it is
 * where the palette hands the whole ranking over to.
 */
function SearchButton() {
  const pathname = usePathname();
  const palette = usePaletteOpen();

  // The palette wins while it is open, wherever it was opened from, because it
  // is the thing in front of everything else.
  const on = palette || pathname === "/search" || pathname.startsWith("/search/");

  return (
    <button
      type="button"
      onClick={() => openPalette()}
      aria-label="Search"
      aria-haspopup="dialog"
      aria-expanded={palette}
      className={cn(
        "pointer-events-auto flex size-13 items-center justify-center rounded-full border bg-card/80 shadow-lg backdrop-blur-xl transition-colors duration-200 focus-visible:ring-2 focus-visible:ring-ring/70 focus-visible:outline-none",
        on ? "text-primary" : "text-muted-foreground hover:text-foreground",
      )}
    >
      <Search className="size-[18px] shrink-0" aria-hidden="true" />
    </button>
  );
}
