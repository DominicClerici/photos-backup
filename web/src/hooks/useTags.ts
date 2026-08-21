"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import {
  approveTriage,
  dismissTagProposal,
  embedTags,
  fetchMergedTags,
  fetchTagCounts,
  fetchTagProposals,
  fetchTagWords,
  judgeTags,
  mergeTags,
  triageTags,
  unmergeTags,
  type TagCounts,
  type TagPass,
  type TagProposal,
  type TagWord,
} from "@/lib/api";
import { toast } from "@/components/ui/toast";
import { counted, type Noun } from "@/lib/format";
import { notifyError } from "@/lib/notify";

const WORDS: Noun = { one: "word", many: "words" };
const PHOTOS: Noun = { one: "photo", many: "photos" };

/** One page of a review list. Long enough that most vocabularies are one page. */
const PAGE = 200;

/**
 * The nine numbers the whole screen is laid out from, and the writes that move
 * them.
 *
 * Every write endpoint answers with a fresh copy rather than a bare result, so
 * this is set from the response instead of refetched. That is not an
 * optimisation so much as an ordering guarantee: judging a word changes which
 * list it is on, how many are left to review, and what the clustering would
 * propose, and a second request to find that out would be reporting a moment
 * shortly after the write rather than the write itself.
 */
export function useTagCounts() {
  const [counts, setCounts] = useState<TagCounts | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);

  useEffect(() => {
    const abort = new AbortController();
    fetchTagCounts(abort.signal)
      .then(setCounts)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : "could not read the vocabulary");
      });
    return () => abort.abort();
  }, [attempt]);

  return { counts, setCounts, error, reload: () => retry((n) => n + 1) };
}

/**
 * The two passes, each run as a loop of bounded calls.
 *
 * The server judges or embeds a slice and says how much is left; this calls
 * again until nothing is. A whole vocabulary through the captioner is a couple
 * of minutes, which is too long to hold one request open and nowhere near
 * enough work to deserve a job kind and a worker pool — so the loop is here,
 * where the person who typed it is watching, and every call is resumable.
 *
 * Closing the tab stops it and loses nothing: the resume point is a column.
 */
export function useTagPasses(apply: (counts: TagCounts) => void) {
  const [running, setRunning] = useState<"triage" | "embed" | null>(null);
  const [progress, setProgress] = useState({ done: 0, total: 0 });
  // Set on unmount, and checked between calls. Without it, navigating away
  // during a two-minute pass leaves a loop firing requests at a page nobody is
  // looking at — and, worse, one that goes on calling setState.
  const stopped = useRef(false);
  useEffect(() => () => void (stopped.current = true), []);

  const run = useCallback(
    async (
      kind: "triage" | "embed",
      call: () => Promise<TagPass>,
      left: (counts: TagCounts) => number,
      report: (done: number) => void,
    ) => {
      setRunning(kind);
      let done = 0;
      let total = 0;
      try {
        for (;;) {
          const out = await call();
          const step = out.triaged ?? out.embedded ?? 0;
          done += step;
          total = Math.max(total, done + left(out.counts));
          if (stopped.current) return;
          apply(out.counts);
          setProgress({ done, total });
          // Zero means the service went away mid-pass — the handler writes
          // whatever it learned first and answers 200 — or that there was
          // nothing to do. Either way, looping again would spin.
          if (step === 0 || left(out.counts) === 0) break;
        }
        report(done);
      } catch (err) {
        notifyError(err, kind === "triage" ? "Could not analyse the words" : "Could not compare the words");
      } finally {
        if (!stopped.current) {
          setRunning(null);
          setProgress({ done: 0, total: 0 });
        }
      }
    },
    [apply],
  );

  const analyse = useCallback(
    () =>
      run("triage", triageTags, (c) => c.untriaged, (done) =>
        toast.add({
          title: done > 0 ? `${counted(done, WORDS)} analysed` : "Nothing left to analyse",
          description:
            done > 0
              ? "Nothing has changed yet for anyone searching: read the two lists, move anything that looks wrong, then approve."
              : "Every word in the vocabulary has been judged already.",
        }),
      ),
    [run],
  );

  const compare = useCallback(
    () =>
      run("embed", embedTags, (c) => c.unembedded, (done) =>
        toast.add({
          title: done > 0 ? `${counted(done, WORDS)} compared` : "Nothing left to compare",
          description: "Suggestions are grouped by how near the words sit to each other.",
        }),
      ),
    [run],
  );

  const approve = useCallback(async () => {
    try {
      const out = await approveTriage();
      apply(out.counts);
      toast.add({
        title: `${counted(out.approved ?? 0, WORDS)} approved`,
        description: `The search index was rebuilt over ${counted(out.reindexed ?? 0, PHOTOS)}. Nothing was deleted — every verdict can still be changed.`,
      });
    } catch (err) {
      notifyError(err, "Could not approve");
    }
  }, [apply]);

  return { running, progress, analyse, compare, approve };
}

/**
 * One of the two review lists, and the one thing you can do to a word on it.
 *
 * Words leave the list the moment they are judged, rather than the list being
 * refetched — the same reasoning useMerges applies to a resolved group. This is
 * a list of independent questions, and answering one does not change what any
 * of the others is asking.
 */
export function useTagWords(junk: boolean, search: string, apply: (counts: TagCounts) => void) {
  const [words, setWords] = useState<TagWord[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);
  const [loadingMore, setLoadingMore] = useState(false);

  useEffect(() => {
    const abort = new AbortController();
    setError(null);
    setWords(null);
    fetchTagWords({ junk, q: search, limit: PAGE }, abort.signal)
      .then((page) => {
        setWords(page.words);
        setTotal(page.total);
      })
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : "could not read the vocabulary");
      });
    return () => abort.abort();
  }, [junk, search, attempt]);

  const more = useCallback(async () => {
    if (!words) return;
    setLoadingMore(true);
    try {
      const page = await fetchTagWords({ junk, q: search, limit: PAGE, offset: words.length });
      setWords((current) => [...(current ?? []), ...page.words]);
      setTotal(page.total);
    } catch (err) {
      notifyError(err, "Could not load more words");
    } finally {
      setLoadingMore(false);
    }
  }, [junk, search, words]);

  const judge = useCallback(
    async (ids: number[]) => {
      const chosen = new Set(ids);
      setWords((current) => current?.filter((w) => !chosen.has(w.id)) ?? null);
      setTotal((n) => Math.max(0, n - ids.length));
      try {
        const out = await judgeTags(ids, !junk);
        apply(out.counts);
      } catch (err) {
        notifyError(err, "Could not move that word");
        retry((n) => n + 1);
      }
    },
    [apply, junk],
  );

  return { words, total, error, loadingMore, more, judge, retry: () => retry((n) => n + 1) };
}

/** The clustering, re-run whenever the threshold moves. */
export function useTagProposals(similarity: number, apply: (counts: TagCounts) => void) {
  const [groups, setGroups] = useState<TagProposal[] | null>(null);
  const [unembedded, setUnembedded] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);

  useEffect(() => {
    const abort = new AbortController();
    setError(null);
    setGroups(null);
    fetchTagProposals(similarity, abort.signal)
      .then((out) => {
        setGroups(out.groups);
        setUnembedded(out.unembedded);
      })
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : "could not read the suggestions");
      });
    return () => abort.abort();
  }, [similarity, attempt]);

  const drop = useCallback((canonical: number) => {
    setGroups((current) => current?.filter((g) => g.canonical.id !== canonical) ?? null);
  }, []);

  /**
   * Accepts a group, with whatever was unticked recorded as a rejection.
   *
   * The two travel together because they are one decision. Sending only the
   * accepted half would leave the rejected words to be proposed against the
   * same head on the next run — the correction undone by the thing it corrected.
   */
  const merge = useCallback(
    async (group: TagProposal, canonical: number, members: number[], rejected: number[]) => {
      drop(group.canonical.id);
      try {
        const out = await mergeTags(canonical, members, rejected);
        toast.add({
          title: `${counted(members.length, WORDS)} merged into “${out.canonical}”`,
          description: `${counted(out.reindexed, PHOTOS)} can now be found under it. Undo it on the Merged tab.`,
        });
        const counts = await fetchTagCounts();
        apply(counts);
      } catch (err) {
        notifyError(err, "Could not merge");
        retry((n) => n + 1);
      }
    },
    [apply, drop],
  );

  const dismiss = useCallback(
    async (group: TagProposal) => {
      drop(group.canonical.id);
      try {
        await dismissTagProposal([group.canonical.id, ...group.members.map((m) => m.id)]);
      } catch (err) {
        notifyError(err, "Could not dismiss");
        retry((n) => n + 1);
      }
    },
    [drop],
  );

  return {
    groups,
    unembedded,
    error,
    merge,
    dismiss,
    retry: () => retry((n) => n + 1),
  };
}

/** The log of what has been folded, and the way back out of it. */
export function useMergedTags(apply: (counts: TagCounts) => void) {
  const [groups, setGroups] = useState<TagProposal[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);

  useEffect(() => {
    const abort = new AbortController();
    setError(null);
    setGroups(null);
    fetchMergedTags(abort.signal)
      .then(setGroups)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : "could not read the merged words");
      });
    return () => abort.abort();
  }, [attempt]);

  const unmerge = useCallback(
    async (ids: number[]) => {
      const restored = new Set(ids);
      setGroups(
        (current) =>
          current
            ?.map((g) => ({ ...g, members: g.members.filter((m) => !restored.has(m.id)) }))
            .filter((g) => g.members.length > 0) ?? null,
      );
      try {
        await unmergeTags(ids);
        apply(await fetchTagCounts());
      } catch (err) {
        notifyError(err, "Could not put that word back");
        retry((n) => n + 1);
      }
    },
    [apply],
  );

  return { groups, error, unmerge, retry: () => retry((n) => n + 1) };
}
