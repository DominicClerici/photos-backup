"use client";

import { useEffect, useState } from "react";

import { fetchHealth, type Health } from "@/lib/api";
import { Badge } from "@/components/ui/badge";

const REFRESH_MS = 15_000;

/**
 * The queue, surfaced where it can actually be noticed.
 *
 * A permanently failed derivative is otherwise invisible: the photo is safely
 * archived, so nothing is lost, but its tile never fills in and there is no
 * reason to go looking at /v1/jobs. Silence is only shown when there is nothing
 * to report.
 */
export function JobStatus() {
  const [health, setHealth] = useState<Health | null>(null);
  const [reachable, setReachable] = useState(true);

  useEffect(() => {
    const controller = new AbortController();

    const poll = () =>
      fetchHealth(controller.signal)
        .then((h) => {
          setHealth(h);
          setReachable(true);
        })
        .catch(() => {
          if (!controller.signal.aborted) setReachable(false);
        });

    poll();
    const id = setInterval(poll, REFRESH_MS);
    return () => {
      controller.abort();
      clearInterval(id);
    };
  }, []);

  if (!reachable)
    return (
      <Badge variant="outline" className="ml-auto border-destructive/40 text-destructive">
        server unreachable
      </Badge>
    );
  if (!health) return null;

  return (
    <span className="ml-auto flex gap-2">
      {health.pending_jobs > 0 ? (
        <Badge variant="outline" className="border-primary/40 text-primary">
          processing {health.pending_jobs.toLocaleString()}
        </Badge>
      ) : null}
      {health.failed_jobs > 0 ? (
        <Badge
          variant="outline"
          className="border-destructive/40 text-destructive"
          title="See GET /v1/jobs for the failures"
        >
          {health.failed_jobs.toLocaleString()} failed
        </Badge>
      ) : null}
    </span>
  );
}
