"use client";

import { useEffect, useState } from "react";

/**
 * The command palette, as a thing any code path can ask for.
 *
 * A module-level subscription rather than a context value, for the reason the
 * vault's password prompt is one: it is opened from places that cannot reach
 * each other. A key pressed anywhere in the document, a button in the bar the
 * root layout mounts, and eventually a menu item on a page that has drawn
 * nothing. One palette, opened from anywhere, beats a callback threaded through
 * every component in between. See useVault.
 */

/** What the box should already contain when it opens. */
export type Seed = string;

type Listener = (open: Seed | null) => void;

let listeners: Listener[] = [];
let showing: Seed | null = null;

/**
 * Opens the palette, optionally with something already in the box.
 *
 * The seed is what makes "edit this search" one keystroke from the results
 * page: the sentence that produced them is what somebody wants to change, and
 * an empty box would mean retyping it.
 */
export function openPalette(seed: Seed = ""): void {
  showing = seed;
  for (const listener of listeners) listener(showing);
}

export function closePalette(): void {
  showing = null;
  for (const listener of listeners) listener(null);
}

export function onPalette(listener: Listener): () => void {
  listeners = [...listeners, listener];
  listener(showing);
  return () => {
    listeners = listeners.filter((l) => l !== listener);
  };
}

/**
 * Whether the palette is open, for the one control that has to look different
 * while it is: the search button floating beside the tab bar, which opens the
 * palette rather than going anywhere and would otherwise never light up.
 */
export function usePaletteOpen(): boolean {
  const [open, setOpen] = useState(false);
  useEffect(() => onPalette((seed) => setOpen(seed !== null)), []);
  return open;
}

/**
 * How to say the shortcut on this keyboard, or empty until it is known.
 *
 * Read after mounting rather than during render, because the server has no
 * keyboard: a component that guessed would either be wrong on half the machines
 * or disagree with itself between the HTML and the hydration. Empty is drawn as
 * nothing, which for a hint nobody needs in order to use the button is the
 * right thing to show for one frame.
 */
export function usePaletteShortcut(): string {
  const [label, setLabel] = useState("");
  useEffect(() => {
    const mac = /mac|iphone|ipad|ipod/i.test(navigator.platform || navigator.userAgent);
    setLabel(mac ? "⌘K" : "Ctrl K");
  }, []);
  return label;
}
