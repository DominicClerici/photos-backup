"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { fetchStatus, type Status } from "@/lib/api";

/**
 * How often the page re-reads the server.
 *
 * Ten seconds, which is roughly how long a thumbnail takes: slow enough that
 * the queue count does not flicker between two numbers, fast enough that
 * watching a backlog drain is watching it rather than reloading.
 */
const REFRESH_MS = 10_000;

/**
 * The status page's one source of fact.
 *
 * Polling stops while the tab is in the background and resumes with an
 * immediate read, so a page left open overnight costs nothing and is never
 * showing yesterday's queue in the second before it corrects itself.
 */
export function useStatus() {
  const [status, setStatus] = useState<Status | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [readAt, setReadAt] = useState<Date | null>(null);
  const abort = useRef<AbortController | null>(null);

  const read = useCallback(() => {
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;

    return fetchStatus(controller.signal)
      .then((next) => {
        setStatus(next);
        setError(null);
        setReadAt(new Date());
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        // The last good answer stays on screen behind the banner. A server
        // restarting is the commonest reason to be on this page, and blanking
        // every card for the four seconds it takes would throw away the
        // numbers somebody is here to compare against.
        setError(err instanceof Error ? err.message : "could not reach the server");
      });
  }, []);

  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | null = null;

    const start = () => {
      if (timer !== null) return;
      void read();
      timer = setInterval(() => void read(), REFRESH_MS);
    };
    const stop = () => {
      if (timer === null) return;
      clearInterval(timer);
      timer = null;
    };

    const onVisibility = () => (document.hidden ? stop() : start());
    if (!document.hidden) start();
    document.addEventListener("visibilitychange", onVisibility);

    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      stop();
      abort.current?.abort();
    };
  }, [read]);

  return { status, error, readAt, refresh: read };
}
