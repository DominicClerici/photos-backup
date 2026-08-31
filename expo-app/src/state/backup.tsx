import { notify } from '@photobackup/core';
import { usePermissions } from 'expo-media-library';
import { createContext, use, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { archiveAddress, archiveToken } from '../archive';
import { shouldRunInBackground, syncBackgroundRegistration } from '../background';
import { loadSelection, saveSelection } from '../sharedalbums/selection';
import { DEFAULT_GATE, NO_GATE, readConditions } from '../sync/conditions';
import { DEFAULT_ENGINE_CONFIG, SyncEngine } from '../sync/engine';
import { acquireRun } from '../sync/exclusive';
import { PhotoKitMediaSource } from '../sync/media';
import { sharedQueueStore, type SqliteQueueStore } from '../sync/sqliteStore';
import { HttpTransport } from '../sync/transport';
import {
  emptyCounts,
  errorText,
  isUnauthorized,
  systemClock,
  type Progress,
  type QueueItem,
} from '../sync/types';
import { useArchive } from './archive';

const IDLE: Progress = {
  phase: 'idle',
  activity: 'Idle',
  counts: emptyCounts(),
  retryAt: 0,
};

/** The most log lines kept. Older ones are not worth the memory or the scroll. */
const LOG_LINES = 20;

/**
 * The backup engine, and everything that watches it.
 *
 * This is a provider rather than state inside the Backup tab for one reason: a
 * run outlives the screen that started it. Switching to the gallery mid-backup
 * unmounts the tab, and a `SyncEngine` held in that component's ref would go
 * with it — mid-upload, with a queue row still marked in flight. Held here it
 * keeps running, and the tab is a view of it.
 *
 * The photo-library permission is here too, because the library is the thing
 * the backup reads and there is nothing else in the app that wants it.
 */
export interface BackupState {
  granted: boolean;
  /** iOS "selected photos" — hides shared albums entirely, among other things. */
  limitedAccess: boolean;

  store: SqliteQueueStore | null;
  storeError: string | null;

  progress: Progress;
  failed: QueueItem[];
  logLines: string[];
  runError: string | null;
  /** Why a backup is being held back — charging, Wi-Fi — or null. */
  heldBecause: string | null;
  running: boolean;
  canStart: boolean;

  start(override?: boolean): Promise<void>;
  pause(): void;
  retryFailed(): Promise<void>;
  recheckArchive(): Promise<void>;

  /** The shared albums ticked for backup. Empty until somebody ticks one. */
  chosenIds: string[];
  chooseAlbums(ids: string[]): void;
  toggleAlbum(localId: string): void;

  forgetShared(): Promise<void>;
  /** How many shared rows the last repair reopened, or null if it has not run. */
  forgotten: number | null;
}

const BackupContext = createContext<BackupState | null>(null);

export function useBackup(): BackupState {
  const state = use(BackupContext);
  if (!state) throw new Error('useBackup outside <BackupProvider>');
  return state;
}

export function BackupProvider({ children }: { children: React.ReactNode }) {
  const { config, credential, unpair, refreshStats } = useArchive();

  const [permission, requestPermission] = usePermissions();
  const [store, setStore] = useState<SqliteQueueStore | null>(null);
  const [storeError, setStoreError] = useState<string | null>(null);
  const [progress, setProgress] = useState<Progress>(IDLE);
  const [failed, setFailed] = useState<QueueItem[]>([]);
  const [logLines, setLogLines] = useState<string[]>([]);
  const [runError, setRunError] = useState<string | null>(null);
  const [heldBecause, setHeldBecause] = useState<string | null>(null);
  const [running, setRunning] = useState(false);
  const [chosenIds, setChosenIds] = useState<string[]>(loadSelection);
  const [forgotten, setForgotten] = useState<number | null>(null);

  const engine = useRef<SyncEngine | null>(null);

  const granted = permission?.granted ?? false;
  const limitedAccess = permission?.accessPrivileges === 'limited';

  useEffect(() => {
    if (!permission) return;
    if (!permission.granted && permission.canAskAgain) requestPermission();
  }, [permission, requestPermission]);

  /**
   * A fresh credential clears the last run's error.
   *
   * The one that matters is the 401's: a run that ends in a revoked token
   * writes "pair this phone again" here and then closes the gate. This provider
   * sits above the gate and keeps its state through the pairing, so without
   * this the Backup tab would greet a newly paired phone with the reason the
   * old one stopped.
   */
  useEffect(() => {
    if (credential) setRunError(null);
  }, [credential]);

  useEffect(() => {
    sharedQueueStore()
      .then(setStore)
      .catch((e) => setStoreError(errorText(e)));
  }, []);

  /**
   * Keeps iOS's idea of the background task in step with the switch in
   * settings, and drops it the moment there is no pairing to run under.
   *
   * Declarative rather than done inside the switch's handler, because the
   * registration lives in iOS and the preference lives in a file on this phone,
   * and only one of those survives a reinstall. Reconciling the two whenever
   * either changes is what makes the switch mean what it says a week later.
   */
  useEffect(() => {
    void syncBackgroundRegistration(shouldRunInBackground(config, credential !== null));
  }, [config, credential]);

  const refreshQueue = useCallback(async (queue: SqliteQueueStore) => {
    const [counts, failedItems] = await Promise.all([queue.counts(), queue.failed(20)]);
    setProgress((current) => ({ ...current, counts }));
    setFailed(failedItems);
  }, []);

  useEffect(() => {
    if (!store) return;
    void refreshQueue(store);
  }, [store, refreshQueue]);

  const log = useCallback((line: string) => {
    setLogLines((lines) => [line, ...lines].slice(0, LOG_LINES));
  }, []);

  const start = useCallback(
    async (override = false) => {
      if (!store || !credential || engine.current?.isRunning) return;
      setRunError(null);

      // 100GB heats the phone and drains the battery. Bulk work waits for a
      // charger and Wi-Fi unless the user explicitly says otherwise, and the
      // reason it is waiting is on screen rather than implied by nothing
      // happening.
      const conditions = await readConditions(override ? NO_GATE : DEFAULT_GATE);
      setHeldBecause(conditions.blockedBy);
      if (conditions.blockedBy) return;

      // A background window can be running this same queue — iOS starts one
      // without asking, and on a background launch this provider is mounted
      // alongside it. `engine.current` above only knows about runs this screen
      // started. See src/sync/exclusive.
      const release = acquireRun('foreground');
      if (!release) {
        setRunError('a background backup is running — it will finish on its own');
        return;
      }

      setRunning(true);

      // Inside the try, and the `finally` below is why: everything from here on
      // runs holding the run lock, so a throw while the engine is being built
      // has to reach a release just as a throw from the run itself does.
      try {
        const instance = new SyncEngine(
          {
            store,
            // The ticked albums, read at the moment the run starts rather than
            // held by the source: enumeration happens once per run, and a run
            // already going should finish the set it was started on.
            media: new PhotoKitMediaSource({ albumIds: chosenIds }),
            transport: new HttpTransport(archiveAddress, archiveToken, log),
            clock: systemClock,
            onProgress: setProgress,
            onLog: log,
          },
          { ...DEFAULT_ENGINE_CONFIG, deviceId: credential.deviceId, maxItems: config.maxItems }
        );
        engine.current = instance;

        await instance.run();
      } catch (e) {
        // A refused device is not a run that went wrong, it is a pairing that
        // has to be redone. Dropping the dead credential is what puts the
        // pairing gate back on screen instead of leaving a Start button that
        // cannot work.
        //
        // The notice is not decoration. Under the gate this screen is gone the
        // instant the credential is, so `runError` has nobody to tell — a toast
        // is the only thing that outlives the tab and reaches the pairing form
        // the user is now looking at. The error is still recorded below for
        // when they come back.
        if (isUnauthorized(e)) {
          await unpair();
          notify({
            type: 'error',
            title: 'The archive refused this phone',
            description: 'Pair it again with a fresh code from `photobackup pair`.',
          });
          setRunError(
            `${errorText(e)} — pair this phone again with a fresh code from \`photobackup pair\`.`
          );
        } else {
          setRunError(errorText(e));
        }
      } finally {
        release();
        setRunning(false);
        await refreshQueue(store);
        // The moment the archived count matters most is the moment a run ends,
        // and it is the one moment it is guaranteed to have changed.
        await refreshStats();
      }
    },
    [store, credential, config.maxItems, chosenIds, log, refreshQueue, refreshStats, unpair]
  );

  const pause = useCallback(() => {
    engine.current?.stop();
  }, []);

  const retryFailed = useCallback(async () => {
    if (!store) return;
    await store.resetFailed();
    await refreshQueue(store);
  }, [store, refreshQueue]);

  // Logged rather than announced in the button, because the count is the whole
  // result: "reopened 6,000" and "reopened 0" mean very different things about
  // where a missing photo went, and the next run overwrites the queue counts
  // this would otherwise have to be read from.
  const recheckArchive = useCallback(async () => {
    if (!store) return;
    const reopened = await store.reopenDone();
    await refreshQueue(store);
    log(`reopened ${reopened} finished item(s) to re-check against the archive`);
  }, [store, refreshQueue, log]);

  /**
   * Drops every shared row from the queue so the next run offers them again.
   *
   * The repair for photographs archived before the phone could name the album
   * they came out of. `done` is the one state nothing else can leave, and the
   * album title only ever arrives on a fresh row, so a re-run alone changes
   * nothing. It costs no bytes: each item comes back as pending, the archive
   * answers `have` from the mapping it already holds, and the item settles
   * straight back to done — describing itself on the way past, which is the
   * point.
   */
  const forgetShared = useCallback(async () => {
    if (!store) return;
    setForgotten(await store.forgetShared());
    await refreshQueue(store);
  }, [store, refreshQueue]);

  /**
   * Nothing is in by default. See `src/sharedalbums/selection.ts`: while this
   * was a survey an unanswered question could mean "all of them" for free, and
   * now that ticking an album uploads it, it cannot.
   */
  const chooseAlbums = useCallback((ids: string[]) => {
    saveSelection(ids);
    setChosenIds(ids);
  }, []);

  const toggleAlbum = useCallback(
    (localId: string) => {
      setChosenIds((current) => {
        const next = current.includes(localId)
          ? current.filter((id) => id !== localId)
          : [...current, localId];
        saveSelection(next);
        return next;
      });
    },
    []
  );

  const canStart = Boolean(store) && granted && Boolean(archiveAddress()) && Boolean(credential) && !running;

  const state = useMemo<BackupState>(
    () => ({
      granted,
      limitedAccess,
      store,
      storeError,
      progress,
      failed,
      logLines,
      runError,
      heldBecause,
      running,
      canStart,
      start,
      pause,
      retryFailed,
      recheckArchive,
      chosenIds,
      chooseAlbums,
      toggleAlbum,
      forgetShared,
      forgotten,
    }),
    [
      granted,
      limitedAccess,
      store,
      storeError,
      progress,
      failed,
      logLines,
      runError,
      heldBecause,
      running,
      canStart,
      start,
      pause,
      retryFailed,
      recheckArchive,
      chosenIds,
      chooseAlbums,
      toggleAlbum,
      forgetShared,
      forgotten,
    ]
  );

  return <BackupContext value={state}>{children}</BackupContext>;
}
