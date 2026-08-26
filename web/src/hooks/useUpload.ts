"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { ApiError, checkContent, uploadAsset, type ContentWhere } from "@/lib/api";
import { hashFile } from "@/lib/hash";
import {
  describeDuplicate,
  inspect,
  kindOf,
  type FileKind,
  type RejectionCode,
} from "@/lib/upload";

/**
 * A file's whole life on the upload page, in one word.
 *
 * The five terminal states are what the page is for. `rejected` and `duplicate`
 * are both "this is not going to be sent" and are deliberately not the same
 * word: one is a mistake to fix, and the other is the archive doing its job.
 */
export type ItemStatus =
  | "checking"
  | "ready"
  | "rejected"
  | "duplicate"
  | "queued"
  | "sending"
  | "stored"
  | "failed";

export interface UploadItem {
  /** Identity for React and for every action on a row. A file has none of its
   * own — two photographs can share a name, a size and a date. */
  key: string;
  file: File;
  kind: FileKind | null;
  status: ItemStatus;
  /**
   * Bytes read while checking, bytes acknowledged while sending, and nothing
   * either side of those. One field rather than two because a row only ever
   * has one bar and it always means the same thing: how far through this file.
   */
  progress: number;
  sha256?: string;
  /** The sentence under the filename, for every state that has something to say. */
  reason?: string;
  code?: RejectionCode;
  where?: ContentWhere;
  /** What it was archived as, once it has been. */
  assetId?: string;
}

export interface UploadSummary {
  total: number;
  checking: number;
  ready: number;
  rejected: number;
  duplicate: number;
  stored: number;
  failed: number;
  /**
   * Everything the button would send if it were pressed now: what passed, plus
   * anything a previous run left failed. A retry is not a different operation.
   */
  sendable: number;
  /** Bytes of that set. */
  sendableBytes: number;
  /** Whether a run is in progress, which is what the buttons key off. */
  running: boolean;
  /**
   * The current run, counted in files rather than bytes because that is what
   * the line above the bar says. Null when there is not one.
   */
  run: { done: number; total: number } | null;
  /** 0–1 across the current run, by bytes, or null when there is not one. */
  progress: number | null;
}

/**
 * How many uploads are in the air at once.
 *
 * Three, over a connection that is almost always loopback. One would leave the
 * link idle during every commit — photod writes a blob, a manifest line and a
 * row between the last byte and the response — and a dozen would only make a
 * single disk seek between twelve files while making every row's bar mean less.
 */
const CONCURRENCY = 3;

/** What one press of Upload came to, for the one sentence that reports it. */
export interface RunResult {
  stored: number;
  failed: number;
  /** Found to be already archived during the send rather than before it. */
  duplicate: number;
}

/**
 * How many digests are asked about at once.
 *
 * Files are hashed one at a time, so waiting for all of them before asking
 * about any would leave two hundred rows spinning through a batch that takes a
 * minute to read. Twenty-five is about a second of hashing on ordinary photos:
 * enough that the page fills in visible waves rather than one row at a time,
 * and small enough that the first wave lands almost immediately.
 */
const CHECK_BATCH = 25;

let nextKey = 0;

/**
 * The upload batch: what is in it, what is wrong with any of it, and what
 * happens when somebody presses the button.
 *
 * Intake and sending are two separate passes, and keeping them separate is the
 * whole shape of the page. Everything a file can be turned away for — its type,
 * its size, the archive already having it — is settled while it is only being
 * read, so pressing Upload sends a set that has already been vetted rather than
 * discovering four hundred megabytes in that it should not have.
 */
export function useUpload() {
  const [items, setItems] = useState<UploadItem[]>([]);
  const [running, setRunning] = useState(false);
  const [run, setRun] = useState<{ done: number; total: number } | null>(null);

  // Every in-flight request, so removing one row stops one transfer and Cancel
  // stops all of them. Keyed by row, because the row is what somebody clicks.
  const inFlight = useRef(new Map<string, AbortController>());
  // What the current run is measured against. Captured when it starts so that
  // adding files mid-run does not make the bar go backwards.
  const runBytes = useRef(0);

  /**
   * The list, kept in a ref beside the state that renders it.
   *
   * Not a cache and not an optimisation: a queued upload of two hundred files
   * has to be able to ask, between one file and the next, whether a row is
   * still on the page. React's updater runs at render time, so reading the
   * answer out of `items` inside a loop that started three files ago reads a
   * list from before any of them finished.
   */
  const known = useRef<UploadItem[]>([]);

  const write = useCallback((fn: (current: UploadItem[]) => UploadItem[]) => {
    known.current = fn(known.current);
    setItems(known.current);
  }, []);

  const patch = useCallback(
    (key: string, change: Partial<UploadItem>) => {
      write((current) =>
        current.map((item) => (item.key === key ? { ...item, ...change } : item)),
      );
    },
    [write],
  );

  useEffect(() => {
    const requests = inFlight.current;
    return () => {
      for (const controller of requests.values()) controller.abort();
    };
  }, []);

  const add = useCallback(
    (files: readonly File[]) => {
      if (files.length === 0) return;

      const fresh: UploadItem[] = files.map((file) => {
        const rejection = inspect(file);
        return {
          key: `f${nextKey++}`,
          file,
          kind: kindOf(file.name),
          status: rejection ? "rejected" : "checking",
          progress: 0,
          reason: rejection?.reason,
          code: rejection?.code,
        };
      });

      write((current) => [...current, ...fresh]);
      void intake(
        fresh.filter((item) => item.status === "checking"),
        patch,
        inFlight.current,
      );
    },
    [patch, write],
  );

  const remove = useCallback(
    (key: string) => {
      inFlight.current.get(key)?.abort();
      inFlight.current.delete(key);
      write((current) => current.filter((item) => item.key !== key));
    },
    [write],
  );

  const reset = useCallback(() => {
    for (const controller of inFlight.current.values()) controller.abort();
    inFlight.current.clear();
    write(() => []);
    setRunning(false);
    setRun(null);
  }, [write]);

  const cancel = useCallback(() => {
    for (const controller of inFlight.current.values()) controller.abort();
    inFlight.current.clear();
  }, []);

  /**
   * Sends everything that passed, and returns what happened so the caller can
   * say so once rather than once per file.
   *
   * The queue is read from a ref-free snapshot taken here: a run is over the
   * set that was ready when the button was pressed, and a file dropped while it
   * is going is the next run's business.
   */
  const start = useCallback(async (): Promise<RunResult> => {
    const queue = known.current.filter(
      (item) => item.status === "ready" || item.status === "failed",
    );
    if (queue.length === 0) return { stored: 0, failed: 0, duplicate: 0 };

    const queued = new Set(queue.map((item) => item.key));
    write((current) =>
      current.map((item) =>
        queued.has(item.key) ? { ...item, status: "queued", progress: 0, reason: undefined } : item,
      ),
    );

    runBytes.current = queue.reduce((sum, item) => sum + item.file.size, 0);
    setRun({ done: 0, total: queue.length });
    setRunning(true);

    let stored = 0;
    let failed = 0;
    let duplicate = 0;
    let at = 0;

    const worker = async () => {
      for (;;) {
        const item = queue[at++];
        if (!item) return;
        // Removed from the page while it waited for a slot. Nothing to send and
        // nothing to report — the row it would have reported into is gone.
        if (!known.current.some((row) => row.key === item.key)) continue;

        const controller = new AbortController();
        inFlight.current.set(item.key, controller);
        patch(item.key, { status: "sending", progress: 0 });

        try {
          const result = await uploadAsset(item.file, {
            sha256: item.sha256,
            signal: controller.signal,
            onProgress: (sent) => patch(item.key, { progress: sent }),
          });
          if (result.duplicate) {
            // Nothing was added: the archive already had these bytes. The check
            // before the run said otherwise, so this is the window between the
            // two closing — the phone uploading the same photograph in the
            // background, or a second tab of this page.
            //
            // Where it is is not reported, and is not guessed. The upload
            // answers "already had it" and nothing more; saying "in the
            // library" would be right most of the time and wrong in exactly the
            // case somebody would want to know about.
            duplicate++;
            patch(item.key, {
              status: "duplicate",
              assetId: result.id,
              progress: item.file.size,
              reason: "The archive already had these bytes, so nothing was added.",
            });
          } else {
            stored++;
            patch(item.key, {
              status: "stored",
              assetId: result.id,
              progress: item.file.size,
            });
          }
        } catch (err) {
          if (aborted(err)) {
            // Somebody pressed Cancel or removed the row. Back to where it was,
            // not to a failure — nothing went wrong with the file.
            patch(item.key, { status: "ready", progress: 0 });
          } else {
            failed++;
            patch(item.key, { status: "failed", progress: 0, reason: message(err) });
          }
        } finally {
          inFlight.current.delete(item.key);
          setRun((current) => (current ? { ...current, done: current.done + 1 } : current));
        }
      }
    };

    await Promise.all(Array.from({ length: Math.min(CONCURRENCY, queue.length) }, worker));
    setRunning(false);
    setRun(null);
    return { stored, failed, duplicate };
  }, [patch, write]);

  const summary = useMemo<UploadSummary>(() => {
    const count = (status: ItemStatus) => items.filter((item) => item.status === status).length;

    let done = 0;
    let inRun = 0;
    for (const item of items) {
      if (item.status === "sending") {
        done += item.progress;
        inRun += item.file.size;
      } else if (item.status === "queued") {
        inRun += item.file.size;
      }
    }
    // Everything already finished this run is the difference between what the
    // run set out to move and what is left in it.
    const moved = running ? runBytes.current - inRun + done : 0;

    const sendable = items.filter(
      (item) => item.status === "ready" || item.status === "failed",
    );

    return {
      total: items.length,
      checking: count("checking"),
      ready: count("ready"),
      rejected: count("rejected"),
      duplicate: count("duplicate"),
      stored: count("stored"),
      failed: count("failed"),
      sendable: sendable.length,
      sendableBytes: sendable.reduce((sum, item) => sum + item.file.size, 0),
      running,
      run,
      progress: running && runBytes.current > 0 ? Math.min(1, moved / runBytes.current) : null,
    };
  }, [items, running, run]);

  return { items, summary, add, remove, reset, start, cancel };
}

/**
 * Reading and vetting a set of files, in the background, while the page shows
 * every one of them as a row.
 *
 * Hashing is sequential on purpose. It is disk-bound rather than CPU-bound —
 * the digest runs at roughly half a gigabyte a second — so reading four files
 * at once would finish all four at about the same late moment instead of
 * finishing the first one early.
 */
async function intake(
  items: UploadItem[],
  patch: (key: string, change: Partial<UploadItem>) => void,
  inFlight: Map<string, AbortController>,
): Promise<void> {
  // Digests seen in this page's whole batch, so the same photograph dropped
  // twice is caught before either copy is sent.
  const pending: UploadItem[] = [];

  const flush = async () => {
    const batch = pending.splice(0, pending.length);
    if (batch.length === 0) return;
    try {
      const known = await checkContent(batch.map((item) => item.sha256!));
      const byDigest = new Map(known.map((match) => [match.sha256, match]));
      for (const item of batch) {
        const match = byDigest.get(item.sha256!);
        if (match) {
          patch(item.key, {
            status: "duplicate",
            where: match.where,
            assetId: match.id,
            // The archived name only when it is news. A row headed beach.jpg
            // that reads "already in the library as beach.jpg" says one thing
            // twice and buries the one case worth reading — the same photograph
            // filed under a name somebody will not recognise.
            reason: describeDuplicate(
              match.where,
              match.filename === item.file.name ? undefined : match.filename,
            ),
          });
        } else {
          patch(item.key, { status: "ready" });
        }
      }
    } catch (err) {
      // The check is an optimisation, not a gate: photod reports a duplicate on
      // the upload itself either way. A server that cannot answer must not turn
      // into a page full of files nobody can send.
      if (!aborted(err)) {
        for (const item of batch) patch(item.key, { status: "ready" });
      }
    }
  };

  const digests = new Map<string, string>();

  for (const item of items) {
    const controller = new AbortController();
    inFlight.set(item.key, controller);
    try {
      const sha256 = await hashFile(
        item.file,
        (read) => patch(item.key, { progress: read }),
        controller.signal,
      );
      patch(item.key, { sha256, progress: item.file.size });

      const twin = digests.get(sha256);
      if (twin) {
        patch(item.key, {
          status: "duplicate",
          code: "duplicate-in-batch",
          reason: `The same file is already in this batch as ${twin}.`,
        });
        continue;
      }
      digests.set(sha256, item.file.name);
      pending.push({ ...item, sha256 });
      if (pending.length >= CHECK_BATCH) await flush();
    } catch (err) {
      if (aborted(err)) return;
      patch(item.key, {
        status: "rejected",
        code: "unreadable",
        reason: `Could not read the file: ${message(err)}`,
      });
    } finally {
      inFlight.delete(item.key);
    }
  }
  await flush();
}

function aborted(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

function message(err: unknown): string {
  if (err instanceof ApiError || err instanceof Error) return err.message;
  return "the server did not answer";
}
