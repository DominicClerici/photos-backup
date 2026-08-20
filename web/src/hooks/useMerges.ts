"use client";

import { useCallback, useEffect, useState } from "react";

import {
  approveGroup,
  dismissGroup,
  fetchMergeGroups,
  forceJoin,
  mergeGroup,
  scanForMerges,
  unmergeGroup,
  undoDelete,
  type MergeGroup,
  type MergeKind,
} from "@/lib/api";
import { toast } from "@/components/ui/toast";
import { counted, ITEMS, type Noun } from "@/lib/format";
import { notifyError, UNDO_MS } from "@/lib/notify";

/** The nouns this page counts in. See format.Noun for why they are pairs. */
const GROUPS: Noun = { one: "group", many: "groups" };
const CLIPS: Noun = { one: "clip", many: "clips" };
const RECORDINGS: Noun = { one: "recording", many: "recordings" };

/**
 * The review, loaded and resolved.
 *
 * Groups are dropped from the list the moment their answer lands rather than by
 * refetching the page. That is the opposite of what a timeline does after a
 * delete — see useTrashActions, where the reload is not optional — and the
 * difference is what the two are lists *of*. A timeline is positions, and a
 * delete moves every position after it; this is a list of independent
 * questions, and answering the fourth one does not change what the fifth is
 * asking. Refetching here would only redraw four hundred thumbnails to remove
 * one row, and would scroll the page out from under somebody halfway down it.
 *
 * The joined recordings tab is two lists in one request each, concatenated: the
 * joins that gave up, then the joins that worked. They are one list on screen
 * because they are one subject — what happened to the recordings Snapchat cut
 * up — and the failures come first because they are the only rows on that page
 * where anything is still owed.
 */
export function useMerges(kind: MergeKind, showApproved = false) {
  const [groups, setGroups] = useState<MergeGroup[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);
  const [scanning, setScanning] = useState(false);

  useEffect(() => {
    const abort = new AbortController();
    setError(null);
    setGroups(null);

    // Pending duplicates are the review; joined recordings are already done —
    // or already given up on — so the lists worth showing of those are what was
    // merged and what failed.
    const load =
      kind === "duplicate"
        ? fetchMergeGroups(kind, "pending", { signal: abort.signal })
        : Promise.all([
            fetchMergeGroups(kind, "failed", { signal: abort.signal }),
            fetchMergeGroups(kind, "merged", {
              approved: showApproved,
              signal: abort.signal,
            }),
          ]).then(([failed, merged]) => [...failed, ...merged]);

    load.then(setGroups).catch((err: unknown) => {
      if (abort.signal.aborted) return;
      setError(err instanceof Error ? err.message : "could not load the review");
    });
    return () => abort.abort();
  }, [kind, showApproved, attempt]);

  const drop = useCallback((id: string) => {
    setGroups((current) => current?.filter((g) => g.id !== id) ?? null);
  }, []);

  const patch = useCallback((id: string, change: Partial<MergeGroup>) => {
    setGroups(
      (current) => current?.map((g) => (g.id === id ? { ...g, ...change } : g)) ?? null,
    );
  }, []);

  /**
   * Keeps one copy of a group and trashes the rest.
   *
   * The toast offers the ordinary delete's Undo, because that is what this
   * ordinarily is: the copies went to Recently Deleted with a batch, and
   * restoring the batch puts them back. What it does not put back is the
   * group — the albums and the caption have already moved onto the keeper, and
   * the question has been answered. Somebody who wants to be asked again can
   * restore and let the next scan find them.
   */
  const merge = useCallback(
    async (group: string, keeper: string) => {
      try {
        const { batch, trashed } = await mergeGroup(group, keeper);
        drop(group);
        const id: string = toast.add({
          title: `${counted(trashed, ITEMS)} merged`,
          description: "The copies are in Recently Deleted for 365 days.",
          timeout: UNDO_MS,
          actionProps: {
            children: "Undo",
            onClick: () => {
              toast.close(id);
              undoDelete(batch)
                .then(() => retry((n) => n + 1))
                .catch((err: unknown) => notifyError(err, "Could not undo"));
            },
          },
        });
      } catch (err) {
        notifyError(err, "Could not merge");
      }
    },
    [drop],
  );

  const dismiss = useCallback(
    async (group: string) => {
      try {
        await dismissGroup(group);
        drop(group);
      } catch (err) {
        notifyError(err, "Could not dismiss");
      }
    },
    [drop],
  );

  /** Takes a joined recording apart again, putting its pieces back. */
  const unmerge = useCallback(
    async (group: string) => {
      try {
        const { restored } = await unmergeGroup(group);
        drop(group);
        toast.add({
          title: `${counted(restored, CLIPS)} put back`,
          description: "The joined recording is in Recently Deleted.",
        });
      } catch (err) {
        notifyError(err, "Could not undo the join");
      }
    },
    [drop],
  );

  /**
   * Signs off one entry of the log, or takes that back.
   *
   * Approving drops the row unless approved rows are being shown, which is the
   * only reason this is not simply a patch: the list somebody is looking at is
   * "what is left to look at", and leaving the row on it would make the button
   * appear not to have worked.
   */
  const approve = useCallback(
    async (group: string, approved: boolean) => {
      try {
        await approveGroup(group, approved);
        if (approved && !showApproved) {
          drop(group);
          return;
        }
        patch(group, { approved_at: approved ? new Date().toISOString() : undefined });
      } catch (err) {
        notifyError(err, approved ? "Could not approve" : "Could not unapprove");
      }
    },
    [drop, patch, showApproved],
  );

  /**
   * Archives a join the server refused to make.
   *
   * The row goes, because what it was — a question about a failure — has been
   * answered. What replaces it is a minute of ffmpeg in the worker pool, so the
   * joined recording appears on this same list a moment later, on the next load
   * rather than in place: there is nothing to show until the file exists.
   */
  const force = useCallback(
    async (group: string) => {
      try {
        await forceJoin(group);
        drop(group);
        toast.add({
          title: "Joining anyway",
          description:
            "The recording is being put back together now, and will appear here when it is done.",
        });
      } catch (err) {
        notifyError(err, "Could not save it anyway");
      }
    },
    [drop],
  );

  /**
   * Looks over the whole library again, and reloads from what it found.
   *
   * Slow enough to need the spinner — a few seconds over a library this size —
   * and worth waiting for rather than backgrounding, because the only reason to
   * press it is to see the result.
   */
  const rescan = useCallback(async () => {
    setScanning(true);
    try {
      const found = await scanForMerges();
      retry((n) => n + 1);
      const total = found.duplicates + found.segments;
      toast.add({
        title: total > 0 ? `${counted(total, GROUPS)} found` : "Nothing new",
        description:
          found.signed < found.assets
            ? `${found.signed.toLocaleString()} of ${found.assets.toLocaleString()} photos have been analysed so far.`
            : found.queued > 0
              ? `${counted(found.queued, RECORDINGS)} queued to be joined.`
              : "Every photo in the library has been compared.",
      });
    } catch (err) {
      notifyError(err, "Could not scan");
    } finally {
      setScanning(false);
    }
  }, []);

  return {
    groups,
    error,
    scanning,
    merge,
    dismiss,
    unmerge,
    approve,
    force,
    rescan,
    retry: () => retry((n) => n + 1),
  };
}
