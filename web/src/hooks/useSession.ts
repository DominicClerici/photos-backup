"use client";

import { useCallback, useEffect, useState } from "react";

import {
  fetchSession,
  onSignedOut,
  signIn as postSession,
  signOut as deleteSession,
  type SessionStatus,
} from "@/lib/session";

/**
 * Whether this browser may see the archive.
 *
 * Three states worth distinguishing, and the middle one is why `ready` exists:
 * before the first answer lands nothing is known, and drawing a password prompt
 * during that moment would flash one in front of every load — including the
 * development gallery, which never needs one.
 *
 * Part of the browser gate; see `@/lib/session` for the whole of what removing
 * it means.
 */
export interface SessionState {
  status: SessionStatus | null;
  ready: boolean;
  /** True only when the server wants a password and does not have one. */
  locked: boolean;
  signIn: (password: string) => Promise<void>;
  signOut: () => Promise<void>;
  refresh: () => void;
}

export function useSession(): SessionState {
  const [status, setStatus] = useState<SessionStatus | null>(null);
  const [ready, setReady] = useState(false);
  const [tick, setTick] = useState(0);

  const refresh = useCallback(() => setTick((n) => n + 1), []);

  useEffect(() => {
    const abort = new AbortController();
    fetchSession(abort.signal)
      .then((next) => {
        setStatus(next);
        setReady(true);
      })
      .catch((err) => {
        if (abort.signal.aborted) return;
        // photod being unreachable is not "signed out". The gallery's own
        // requests will fail visibly and say so; putting a password prompt up
        // for a server that is down would just be a lie about which thing is
        // broken.
        setReady(true);
        void err;
      });
    return () => abort.abort();
  }, [tick]);

  // A 401 from anywhere means the session lapsed between two requests. Asking
  // again rather than assuming: the same 401 is what an unpaired device gets,
  // and only the server can tell the two apart.
  useEffect(() => onSignedOut(refresh), [refresh]);

  // A laptop that was shut for a fortnight comes back to a session that has
  // expired, and nothing tells the tab. Checking on focus costs one request per
  // return to the window and saves a screen full of failed thumbnails.
  useEffect(() => {
    const onFocus = () => refresh();
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refresh]);

  const signIn = useCallback(async (password: string) => {
    setStatus(await postSession(password));
  }, []);

  const signOut = useCallback(async () => {
    await deleteSession();
    setStatus((prev) => (prev ? { ...prev, signedIn: false, expiresAt: null } : prev));
  }, []);

  return {
    status,
    ready,
    locked: ready && status !== null && status.required && !status.signedIn,
    signIn,
    signOut,
    refresh,
  };
}
