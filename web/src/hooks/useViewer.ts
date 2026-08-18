"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import type { TimelineState } from "./useTimeline";

export interface ViewerState {
  /** Position of the open asset in the timeline, or -1 when none is open. */
  index: number;
  open: (id: string) => void;
  close: () => void;
  navigate: (next: number) => void;
}

/**
 * Which asset the viewer is showing, kept in the URL so it can be linked to.
 *
 * The open asset is `?asset=<id>` on whatever page is doing the browsing, so
 * this works the same over the library and over one collection — the path says
 * what is being browsed, the query says where in it.
 */
export function useViewer(timeline: TimelineState): ViewerState {
  const [openID, setOpenID] = useState<string | null>(null);

  // Whether *we* added the history entry. A viewer opened from the grid should
  // close on Back; one opened from a pasted link has nothing behind it, and
  // calling back() there would navigate away from the site entirely.
  const pushed = useRef(false);

  useEffect(() => {
    const readURL = () =>
      setOpenID(new URLSearchParams(window.location.search).get("asset"));
    readURL();
    window.addEventListener("popstate", readURL);
    return () => window.removeEventListener("popstate", readURL);
  }, []);

  const { at, indexOf, locate, request } = timeline;
  const index = openID ? indexOf(openID) : -1;

  // Where a linked photo is, once the server has been asked. -1 covers both
  // "nothing to ask about" and "this timeline does not hold it", which are the
  // same thing as far as opening the viewer goes.
  const [linked, setLinked] = useState(-1);

  // A link to an older photo arrives before the page holding it, and a link is
  // the one thing here that names a photograph by id rather than by position.
  // So the position is asked for directly — one count on the server — rather
  // than paged towards, which used to cost a request per two hundred items
  // ahead of it and was the last place in the gallery that walked.
  useEffect(() => {
    if (!openID || index !== -1) {
      setLinked(-1);
      return;
    }
    const abort = new AbortController();
    locate(openID, abort.signal)
      .then(setLinked)
      // Leaving the grid up with the link in the URL. There is no page to fall
      // back to paging towards any more, and pretending otherwise would mean
      // fetching the entire library to answer a question that just failed.
      .catch(() => setLinked(-1));
    return () => abort.abort();
  }, [openID, index, locate]);

  // Holding the page that photo is on until it arrives, and letting go the
  // moment it does — the viewer's own range takes over from there.
  useEffect(() => {
    if (linked < 0) {
      request("locate", 0, 0);
      return;
    }
    request("locate", linked, linked + 1);
  }, [linked, request]);

  // Arrow-keying toward a photo that has not been fetched pulls it in, along
  // with the neighbours the viewer preloads.
  useEffect(() => {
    if (index < 0) {
      request("viewer", 0, 0);
      return;
    }
    request("viewer", index - 1, index + 2);
  }, [index, request]);

  const open = useCallback((id: string) => {
    setOpenID(id);
    window.history.pushState(null, "", `?asset=${id}`);
    pushed.current = true;
  }, []);

  const close = useCallback(() => {
    if (pushed.current) {
      pushed.current = false;
      // popstate does the state change, keeping the URL and the overlay in step.
      window.history.back();
      return;
    }
    window.history.replaceState(null, "", window.location.pathname);
    setOpenID(null);
  }, []);

  // Stepping through photos replaces the entry instead of adding one, so Back
  // closes the viewer rather than walking back through everything just viewed.
  const navigate = useCallback(
    (next: number) => {
      const target = at(next);
      if (!target) return;
      setOpenID(target.id);
      window.history.replaceState(null, "", `?asset=${target.id}`);
    },
    [at],
  );

  return { index, open, close, navigate };
}
