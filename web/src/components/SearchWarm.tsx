"use client";

import { useEffect } from "react";

import { warmSearch } from "@/lib/api";

/**
 * Tells the archive that a gallery is open, so the models a search needs start
 * loading before anybody types.
 *
 * Renders nothing, and is mounted in the root layout beside the other things
 * that have to outlive a page: this has to fire on the first load of any route,
 * not only on /search, because the whole point is that the twenty seconds of
 * checkpoint happen while somebody is looking at their photographs rather than
 * after they have asked a question.
 *
 * photod keeps the lease alive on ordinary gallery traffic after that — every
 * timeline page and every thumbnail is evidence that this tab is still open —
 * so there is nothing to poll and nothing to tear down. What is left for this
 * component is the two moments that traffic does not cover: the first paint,
 * and coming back to a tab that has been in the background long enough for the
 * lease to have lapsed.
 *
 * Failures are swallowed on purpose. There is nothing to tell anybody: a search
 * without the models still returns photographs, ranked by captions, tags,
 * recognised text, filenames and place names, and the search page says so when
 * it happens.
 */
export function SearchWarm() {
  useEffect(() => {
    const warm = () => {
      void warmSearch().catch(() => {});
    };
    warm();

    // Not `focus`, which fires on every click back into the window. This is the
    // question "was this tab hidden and is it now not", which is the one that
    // corresponds to a lease that may have lapsed.
    const onVisible = () => {
      if (document.visibilityState === "visible") warm();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, []);

  return null;
}
