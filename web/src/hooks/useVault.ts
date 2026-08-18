"use client";

import { useCallback, useEffect, useState } from "react";

import {
  ApiError,
  fetchVaultStatus,
  lockVault,
  LOCKED,
  NO_VAULT,
  setupVault,
  unlockVault,
  type Bucket,
  type VaultStatus,
} from "@/lib/api";

/** What each bucket is called, in the one place both of them are named. */
export const BUCKET_LABEL: Record<Bucket, string> = {
  archive: "Archive",
  hidden: "Hidden",
};

/** The verb each bucket's action carries. "Archive photo", "Hide 12 items". */
export const BUCKET_VERB: Record<Bucket, string> = {
  archive: "Archive",
  hidden: "Hide",
};

/**
 * The password prompt, as a thing any code path can ask for.
 *
 * It is a module-level subscription rather than a prop or a context value
 * because of where it gets asked for from: a right-click in the grid, a menu on
 * an album tile, a page that has just discovered it is locked. All three are
 * three different components deep in three different trees, and threading a
 * callback from the layout down to each of them would be a prop that exists
 * only to be forwarded.
 *
 * The same argument the toast singleton makes, for the same reason.
 */
type GateReason = "unlock" | "setup";
type GateListener = (reason: GateReason | null) => void;

let listeners: GateListener[] = [];
let asking: GateReason | null = null;

function ask(reason: GateReason) {
  asking = reason;
  for (const listener of listeners) listener(reason);
}

export function closeGate() {
  asking = null;
  for (const listener of listeners) listener(null);
}

/** Subscribes to the gate. Used by the one component that draws it. */
export function onGate(listener: GateListener): () => void {
  listeners = [...listeners, listener];
  listener(asking);
  return () => {
    listeners = listeners.filter((l) => l !== listener);
  };
}

/**
 * The lock state, broadcast the moment it changes.
 *
 * The poll below is the fallback — the key is dropped after fifteen idle
 * minutes and nothing tells the browser — but a poll is the wrong way to learn
 * about the change somebody just made by hand. The dialog that takes the
 * password is mounted by the root layout and the page waiting on it is
 * somewhere else entirely, so without this, typing the right password leaves
 * the Archive page saying "locked" for up to half a minute.
 */
type StatusListener = (status: VaultStatus) => void;
let watchers: StatusListener[] = [];

function announce(status: VaultStatus) {
  for (const watcher of watchers) watcher(status);
}

function onStatus(watcher: StatusListener): () => void {
  watchers = [...watchers, watcher];
  return () => {
    watchers = watchers.filter((w) => w !== watcher);
  };
}

/**
 * Turns the two statuses that mean "this needs the password" into the prompt,
 * and reports that it did.
 *
 * A caller that gets true has nothing left to say: the failure is already on
 * screen as a dialog, and a toast beside it saying "could not archive" would be
 * telling somebody off for a thing they are in the middle of doing.
 */
export function needsVault(err: unknown): boolean {
  if (!(err instanceof ApiError)) return false;
  if (err.status === NO_VAULT) {
    ask("setup");
    return true;
  }
  if (err.status === LOCKED) {
    ask("unlock");
    return true;
  }
  return false;
}

/** Opens the prompt directly, for the pages that know they are locked. */
export function askToUnlock(status?: VaultStatus | null) {
  ask(status && !status.exists ? "setup" : "unlock");
}

export interface VaultState {
  status: VaultStatus | null;
  /** False until the first poll lands: there is no lock state before that. */
  ready: boolean;
  error: string | null;
  unlock: (password: string) => Promise<void>;
  create: (password: string) => Promise<void>;
  lock: () => Promise<void>;
  refresh: () => void;
}

/**
 * The vault's lock state, polled.
 *
 * Polled rather than pushed because it changes without anybody asking: the key
 * is dropped after fifteen idle minutes, and a page left open across that
 * boundary should notice rather than keep drawing thumbnails that have started
 * answering 423.
 *
 * The poll is deliberately a read of the status endpoint, which does *not*
 * extend the idle window — otherwise an open tab would keep a vault unlocked on
 * an empty desk forever, which is the exact thing the timeout exists to stop.
 */
export function useVault(pollMs = 30_000): VaultState {
  const [status, setStatus] = useState<VaultStatus | null>(null);
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);

  useEffect(() => {
    const abort = new AbortController();
    let live = true;

    const read = () => {
      fetchVaultStatus(abort.signal)
        .then((next) => {
          if (!live) return;
          setStatus(next);
          setReady(true);
        })
        .catch(() => {
          if (live) setReady(true);
        });
    };

    read();
    const timer = window.setInterval(read, pollMs);
    const stop = onStatus((next) => {
      if (live) {
        setStatus(next);
        setReady(true);
      }
    });
    return () => {
      live = false;
      stop();
      abort.abort();
      window.clearInterval(timer);
    };
  }, [attempt, pollMs]);

  const unlock = useCallback(async (password: string) => {
    setError(null);
    try {
      announce(await unlockVault(password));
      closeGate();
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not open the vault");
      throw err;
    }
  }, []);

  const create = useCallback(async (password: string) => {
    setError(null);
    try {
      announce(await setupVault(password));
      closeGate();
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not create the vault");
      throw err;
    }
  }, []);

  const lock = useCallback(async () => {
    announce(await lockVault());
  }, []);

  return {
    status,
    ready,
    error,
    unlock,
    create,
    lock,
    refresh: () => retry((n) => n + 1),
  };
}
