"use client";

/**
 * The selection, plus the one part of it that only makes sense in a browser.
 *
 * The state machine — enter, toggle, span, the drag, the runs it settles into —
 * is in packages/core, expressed in `ranges.ts` and knowing nothing about how a
 * selection is made. What was lifted back out is Escape: a keydown listener and
 * a `document.querySelector`, neither of which a phone has, and which the
 * provider can wrap around the portable one instead of containing.
 */
import { useEffect, type ReactNode } from "react";

import {
  SelectionProvider as PortableSelectionProvider,
  useSelection,
} from "@photobackup/core/react";

import "@/lib/archive";

export {
  useSelection,
  useSelectionScope,
  type AlbumRef,
  type SelectionActions,
  type SelectionState,
} from "@photobackup/core/react";

export function SelectionProvider({ children }: { children: ReactNode }) {
  return (
    <PortableSelectionProvider>
      <Escape />
      {children}
    </PortableSelectionProvider>
  );
}

/**
 * Escape leaves the selection, unless something is over the grid.
 *
 * Escape in a dialog or a menu means "never mind, close this" — throwing the
 * selection away as well would undo the very thing somebody was being asked
 * about, and dismissing the album menu is not a reason to lose forty
 * photographs. Rendered by the provider rather than hooked into it so that the
 * listener is mounted exactly where the state is, and nowhere a page has to
 * remember to put it.
 */
function Escape(): null {
  const { active, exit } = useSelection();

  useEffect(() => {
    if (!active) return;
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key !== "Escape") return;
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
  }, [active, exit]);

  return null;
}
