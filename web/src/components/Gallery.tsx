"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { useTimeline } from "@/hooks/useTimeline";
import { Timeline } from "./Timeline";
import { Viewer } from "./Viewer";
import { JobStatus } from "./JobStatus";

export function Gallery() {
  const timeline = useTimeline();
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

  const index = openID ? timeline.items.findIndex((it) => it.id === openID) : -1;

  // A link to an older photo arrives before the page holding it. Paging until it
  // turns up is bounded by the library size and costs ~16KB a page, which beats
  // either a second lookup endpoint or silently refusing to open the link.
  const { hasMore, loading, loadMore, items } = timeline;
  useEffect(() => {
    if (openID && index === -1 && hasMore && !loading) loadMore();
  }, [openID, index, hasMore, loading, loadMore]);

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
      const target = items[next];
      if (!target) return;
      setOpenID(target.id);
      window.history.replaceState(null, "", `?asset=${target.id}`);
    },
    [items],
  );

  // Arrow-keying toward the end of the loaded list pulls the next page in
  // before it is reached.
  useEffect(() => {
    if (index >= 0 && index > items.length - 10 && hasMore && !loading) loadMore();
  }, [index, items.length, hasMore, loading, loadMore]);

  return (
    <div className="app">
      <header className="appBar">
        <h1>Photos</h1>
        <span className="appCount">
          {items.length.toLocaleString()}
          {timeline.hasMore ? "+" : ""} items
        </span>
        <JobStatus />
      </header>

      <Timeline
        items={items}
        loading={timeline.loading}
        hasMore={timeline.hasMore}
        error={timeline.error}
        loadMore={loadMore}
        patch={timeline.patch}
        retry={timeline.retry}
        onOpen={open}
      />

      {index >= 0 ? (
        <Viewer items={items} index={index} onClose={close} onNavigate={navigate} />
      ) : null}
    </div>
  );
}
