/**
 * Where this client's archive is, and what a dead credential means here.
 *
 * The three facts in `Transport` are the whole of what `@photobackup/core`
 * cannot know about the app it is inside. In a browser they are short: photod
 * is the front door and serves `/api` itself, the session is a cookie the fetch
 * attaches without being asked, and a 401 means the session ended.
 *
 * Imported for its side effect, at the top of every module that can start a
 * request — `@/lib/api` and each hook in `@/hooks`. That is deliberate rather
 * than tidy: the alternative is trusting that some component further up the
 * bundle happened to be evaluated first, and a transport that is configured
 * only most of the time is worse than one that is never configured at all.
 */
import { configure } from "@photobackup/core";

configure({
  // photod serves /api/v1/… and /v1/… from the same handler behind a
  // StripPrefix. The browser has always used the first and the phone the
  // second, which is why neither had to be rewritten to land on one.
  baseUrl: () => "/api",

  // Nothing to add. The cookie rides along by itself, which is the entire
  // reason the browser can show a thumbnail at all.
  headers: () => ({}),

  /**
   * Sends the browser to sign in again, remembering where it was.
   *
   * The sign-in page is served by photod rather than by Next — see
   * internal/api/frontdoor.go — which is why this is a location assignment
   * rather than a router push. There is no route on this side to push to.
   *
   * Called at most once per outage; the guard is in core, where the two
   * hundred simultaneous 401s a grid can produce are noticed.
   */
  onUnauthorized: () => {
    if (typeof window === "undefined") return;
    const here = window.location.pathname + window.location.search;
    window.location.assign(`/signin?next=${encodeURIComponent(here)}`);
  },
});
