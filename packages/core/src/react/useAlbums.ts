"use client";

import { useCallback, useEffect, useState } from "react";

import { fetchAlbums, fetchAssetAlbums, type Album, type Bucket } from "../wire/api.ts";
import { needsVault } from "./useVault.ts";

/**
 * The album list, held above the menus that draw it.
 *
 * A module-level cache rather than a fetch per menu, because of where this gets
 * asked for from: every right-click on the grid, every selection sheet, every
 * album tile. The list changes only when somebody makes or deletes an album,
 * which is a thing this app knows about the instant it happens — so it is
 * cached until told otherwise rather than re-read on a timer.
 *
 * Keyed by bucket, because the three lists are three different questions. An
 * album in the Archive is not an album a library photograph can be put into,
 * and drawing the library's albums in a menu opened inside a bucket would offer
 * exactly the move the server refuses.
 */
const cache = new Map<string, Album[]>();

type Listener = () => void;
let listeners: Listener[] = [];

/**
 * Drops the cache and tells every open menu.
 *
 * Called after anything that changes what albums exist or what is in them. The
 * counts are part of what a menu draws, so filling an album invalidates the
 * list as surely as making one does.
 */
export function albumsChanged(): void {
  cache.clear();
  for (const listener of listeners) listener();
}

/**
 * Subscribes to that broadcast from outside a menu.
 *
 * The mobile app keeps an offline copy of the collections index and of the
 * timelines hanging off it, and every write that changes what albums exist or
 * what is in them already comes through here. So this is the one place it can
 * learn that the copy is describing an archive that has moved on — see
 * `expo-app/src/gallery/cache.ts`. The browser has no such copy and never calls
 * this.
 */
export function onAlbumsChanged(listener: () => void): () => void {
  listeners = [...listeners, listener];
  return () => {
    listeners = listeners.filter((l) => l !== listener);
  };
}

export interface AlbumList {
  /** Null until the first read lands. An empty array is an archive with none. */
  albums: Album[] | null;
  error: string | null;
  reload: () => void;
}

/**
 * The albums of one scope, fetched the first time something asks to see them.
 *
 * @param bucket Which half of the archive. Undefined is the library's own.
 * @param enabled False while the menu holding this is shut, which is what keeps
 * a right-click from costing a request nobody opened the submenu to see.
 */
export function useAlbums(bucket: Bucket | undefined, enabled: boolean): AlbumList {
  const key = bucket ?? "library";
  const [albums, setAlbums] = useState<Album[] | null>(() => cache.get(key) ?? null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);

  const reload = useCallback(() => retry((n) => n + 1), []);

  useEffect(() => {
    const listener = () => retry((n) => n + 1);
    listeners = [...listeners, listener];
    return () => {
      listeners = listeners.filter((l) => l !== listener);
    };
  }, []);

  useEffect(() => {
    if (!enabled) return;

    // A held list is used as it stands rather than re-read. Every write that
    // changes what albums exist or what is in them calls albumsChanged, so a
    // stale cache means something outside this browser changed the archive —
    // and paying for a refetch on every menu open to cover that would, in a
    // bucket, mean decrypting the whole vault each time somebody right-clicks.
    const held = cache.get(key);
    if (held) {
      setAlbums(held);
      return;
    }

    const abort = new AbortController();
    setError(null);
    fetchAlbums(bucket, abort.signal)
      .then((found) => {
        cache.set(key, found);
        setAlbums(found);
      })
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        // A locked vault is a password prompt, which needsVault has already put
        // on screen. Saying "could not load albums" beside it would be telling
        // somebody off for a thing they are in the middle of doing.
        if (needsVault(err)) return;
        setError(err instanceof Error ? err.message : "could not load albums");
      });
    return () => abort.abort();
  }, [bucket, key, enabled, attempt]);

  return { albums, error, reload };
}

export interface Membership {
  /** Which albums hold this photograph, or null while that is unknown. */
  held: Set<string> | null;
  /** Records a change this client just made, without re-asking the server. */
  mark: (album: string, member: boolean) => void;
}

/**
 * Which albums one photograph is in, for the ticks in the menu.
 *
 * Only ever asked about one, and deliberately. A selection of forty has forty
 * answers and no useful way to draw them — a tick would have to mean "all of
 * them", a dash would have to mean "some", and neither is a thing anybody wants
 * to read off a menu they opened to file something. So the ticks appear when
 * the menu is about exactly one photograph, and not otherwise.
 *
 * @param assetId The one photograph, or null when the menu is about several.
 */
export function useMembership(assetId: string | null, enabled: boolean): Membership {
  const [held, setHeld] = useState<Set<string> | null>(null);

  useEffect(() => {
    if (!enabled || !assetId) {
      setHeld(null);
      return;
    }
    const abort = new AbortController();
    setHeld(null);
    fetchAssetAlbums(assetId, abort.signal)
      .then((ids) => setHeld(new Set(ids)))
      .catch(() => {
        // No ticks rather than no menu. Not knowing which albums something is
        // already in is a worse menu, not a broken one.
      });
    return () => abort.abort();
  }, [assetId, enabled]);

  const mark = useCallback((album: string, member: boolean) => {
    setHeld((current) => {
      if (!current) return current;
      const next = new Set(current);
      if (member) next.add(album);
      else next.delete(album);
      return next;
    });
  }, []);

  return { held, mark };
}
