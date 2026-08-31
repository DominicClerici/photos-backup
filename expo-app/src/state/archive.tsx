import { notify } from '@photobackup/core';
import { createContext, use, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import {
  archiveAddress,
  archiveToken,
  onCredentialLost,
  setArchiveAddress,
  setArchiveToken,
} from '../archive';
import { DEFAULT_MAX_ITEMS, loadConfig, saveConfig, type Config } from '../config';
import { resolveServer, type ServerResolution } from '../discovery';
import { GalleryClient } from '../gallery/client';
import { clearCredential, loadCredential, pair, type Credential } from '../pairing';
import {
  clearCachedStats,
  loadCachedStats,
  saveCachedStats,
  type CachedStats,
} from '../stats/cache';
import { errorText } from '../sync/types';

/**
 * Where the archive is, whether this phone may read it, and what it holds.
 *
 * All of this was local state in `App.tsx`, which could hold it locally because
 * there was one screen. There are now four, and every one of them needs some of
 * it: the gate needs the credential, the backup tab needs the address, settings
 * needs all of it. So it is lifted exactly once, here, rather than threaded.
 *
 * What is *not* here is the address and the token themselves. Those live in
 * `src/archive.ts` as module state, read per request, because the sync engine's
 * transport and `@photobackup/core`'s both read them from there and neither can
 * see a React context. This provider is the thing that writes them.
 */
export interface ArchiveState {
  config: Config;
  setServerUrl(value: string): void;
  setMaxItems(value: string): void;
  /** Whether iOS may wake the app to back up. See src/background. */
  setBackgroundBackup(value: boolean): void;

  server: ServerResolution | null;
  resolving: boolean;
  findServer(): Promise<void>;

  credential: Credential | null;
  /** False only until the keychain has been read once, which is a flicker. */
  credentialChecked: boolean;
  pairing: boolean;
  pairError: string | null;
  /** True when a credential was stored, so the form knows to clear the code. */
  submitPairing(code: string, deviceName: string): Promise<boolean>;
  unpair(): Promise<void>;

  stats: CachedStats | null;
  /** The last refresh failed and what is on screen is the cache. */
  statsStale: boolean;
  loadingStats: boolean;
  refreshStats(): Promise<void>;

  gallery: GalleryClient;
}

const ArchiveContext = createContext<ArchiveState | null>(null);

export function useArchive(): ArchiveState {
  const state = use(ArchiveContext);
  if (!state) throw new Error('useArchive outside <ArchiveProvider>');
  return state;
}

export function ArchiveProvider({ children }: { children: React.ReactNode }) {
  const [config, setConfig] = useState<Config>(loadConfig);
  const [server, setServer] = useState<ServerResolution | null>(null);
  const [resolving, setResolving] = useState(false);
  const [credential, setCredential] = useState<Credential | null>(null);
  const [credentialChecked, setCredentialChecked] = useState(false);
  const [pairing, setPairing] = useState(false);
  const [pairError, setPairError] = useState<string | null>(null);
  /** Seeded from the cache so the card has numbers before the first fetch. */
  const [stats, setStats] = useState<CachedStats | null>(loadCachedStats);
  const [statsStale, setStatsStale] = useState(false);
  const [loadingStats, setLoadingStats] = useState(false);

  // Seeded once, from the address typed into settings; discovery replaces it
  // the moment it resolves one. Once rather than on every render, which is what
  // useRef's initial value gave for free — repeating it would overwrite a
  // resolved address with the typed-in one on the next state change.
  const seeded = useRef(false);
  if (!seeded.current) {
    seeded.current = true;
    setArchiveAddress(config.serverUrl);
  }

  // Built once and kept: it reads the address and the token per request, so it
  // survives both changing under it.
  const gallery = useRef(new GalleryClient(archiveAddress, archiveToken)).current;

  const statsFetched = useRef(false);

  /**
   * Re-reads what the archive holds.
   *
   * A failure is deliberately not an error on screen. These numbers describe
   * photos that are archived whether or not the phone can reach the server to
   * ask about them, so an unreachable server marks the card stale and leaves
   * the last known figures up rather than blanking it.
   */
  const refreshStats = useCallback(async () => {
    if (!archiveAddress() || !archiveToken()) return;
    setLoadingStats(true);
    try {
      const entry = { fetchedAt: Date.now(), stats: await gallery.stats() };
      setStats(entry);
      saveCachedStats(entry);
      setStatsStale(false);
    } catch {
      setStatsStale(true);
    } finally {
      setLoadingStats(false);
    }
  }, [gallery]);

  const unpair = useCallback(async () => {
    await clearCredential();
    setCredential(null);
    setArchiveToken(null);

    // The cached figures belong to the device that was just forgotten. Left up,
    // they would credit whatever phone pairs next with somebody else's backup.
    clearCachedStats();
    setStats(null);
    setStatsStale(false);
    statsFetched.current = false;
  }, []);

  /**
   * What the app does when photod refuses the token.
   *
   * Under a hard gate this needs no routing: the credential going to null is
   * what puts the pairing screen back on screen, because that is the whole of
   * what the gate in `app/_layout.tsx` tests. The notice is the part that would
   * otherwise be missing — a screen that silently becomes the pairing form
   * looks like the app restarted rather than like the token was revoked.
   */
  useEffect(() => {
    onCredentialLost(() => {
      void unpair();
      notify({
        type: 'error',
        title: 'This phone is no longer paired',
        description:
          'The archive refused its token — it has probably been revoked. Pair again with a fresh code.',
      });
    });
  }, [unpair]);

  useEffect(() => {
    loadCredential()
      .then((found) => {
        setCredential(found);
        setArchiveToken(found?.token ?? null);
      })
      .finally(() => setCredentialChecked(true));
  }, []);

  const findServer = useCallback(async () => {
    setResolving(true);
    try {
      const resolution = await resolveServer({
        rememberedUrl: config.lastServerUrl,
        manualUrl: config.serverUrl.trim() || null,
      });
      setServer(resolution);
      if (!resolution.url) return;

      setArchiveAddress(resolution.url);
      if (resolution.url !== config.lastServerUrl) {
        const next = { ...config, lastServerUrl: resolution.url };
        setConfig(next);
        saveConfig(next);
      }
    } finally {
      setResolving(false);
    }
  }, [config]);

  // One automatic scan on launch; after that it is a button, so a slow or
  // denied scan never blocks the screen repeatedly.
  const scannedOnce = useRef(false);
  useEffect(() => {
    if (scannedOnce.current) return;
    scannedOnce.current = true;
    void findServer();
  }, [findServer]);

  // Once, as soon as there is both an address and a token. On a fresh install
  // that is not at launch at all — the gate stays shut until a pairing
  // succeeds, and the fetch happens then.
  useEffect(() => {
    if (statsFetched.current || !credential || !server?.url) return;
    statsFetched.current = true;
    void refreshStats();
  }, [credential, server, refreshStats]);

  const submitPairing = useCallback(async (code: string, deviceName: string): Promise<boolean> => {
    if (!archiveAddress()) return false;
    setPairing(true);
    setPairError(null);
    try {
      const paired = await pair({ serverUrl: archiveAddress(), code, deviceName });
      setCredential(paired);
      setArchiveToken(paired.token);
      return true;
    } catch (e) {
      setPairError(errorText(e));
      return false;
    } finally {
      setPairing(false);
    }
  }, []);

  const setServerUrl = useCallback((value: string) => {
    setConfig((current) => {
      const next = { ...current, serverUrl: value };
      saveConfig(next);
      return next;
    });
  }, []);

  const setMaxItems = useCallback((value: string) => {
    const parsed = Number.parseInt(value.replace(/[^0-9]/g, ''), 10);
    setConfig((current) => {
      const next = { ...current, maxItems: Number.isFinite(parsed) ? parsed : DEFAULT_MAX_ITEMS };
      saveConfig(next);
      return next;
    });
  }, []);

  const setBackgroundBackup = useCallback((value: boolean) => {
    setConfig((current) => {
      const next = { ...current, backgroundBackup: value };
      saveConfig(next);
      return next;
    });
  }, []);

  const state = useMemo<ArchiveState>(
    () => ({
      config,
      setServerUrl,
      setMaxItems,
      setBackgroundBackup,
      server,
      resolving,
      findServer,
      credential,
      credentialChecked,
      pairing,
      pairError,
      submitPairing,
      unpair,
      stats,
      statsStale,
      loadingStats,
      refreshStats,
      gallery,
    }),
    [
      config,
      setServerUrl,
      setMaxItems,
      setBackgroundBackup,
      server,
      resolving,
      findServer,
      credential,
      credentialChecked,
      pairing,
      pairError,
      submitPairing,
      unpair,
      stats,
      statsStale,
      loadingStats,
      refreshStats,
      gallery,
    ]
  );

  return <ArchiveContext value={state}>{children}</ArchiveContext>;
}
