import { usePermissions } from 'expo-media-library';
import { StatusBar } from 'expo-status-bar';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  ActivityIndicator,
  Pressable,
  SafeAreaView,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';

import { DEFAULT_MAX_ITEMS, loadConfig, saveConfig, type Config } from './src/config';
import { resolveServer, type ServerResolution } from './src/discovery';
import { checkGalleryAccess, type CheckResult } from './src/gallery/check';
import { GalleryClient } from './src/gallery/client';
import { clearCredential, loadCredential, pair, type Credential } from './src/pairing';
import {
  clearCachedStats,
  loadCachedStats,
  saveCachedStats,
  type CachedStats,
} from './src/stats/cache';
import {
  canDownloadShared,
  photoKitSharedProvenance,
  type SharedAsset,
  type SharedProvenance,
} from './modules/photo-facts';
import { fetchSharedAssets } from './src/sharedalbums/fetch';
import type { FetchRun, SampleRead } from './src/sharedalbums/run';
import { loadSelection, saveSelection } from './src/sharedalbums/selection';
import { surveySharedAlbums } from './src/sharedalbums/survey';
import {
  assetsOf,
  formatDuration,
  pickSample,
  SAMPLE_SIZE,
  SAMPLE_SIZES,
  STILL_LONG_EDGE_CAP,
  summarize,
  throughputMbPerSecond,
  VIDEO_SECONDS_CAP,
  type AlbumSummary,
  type SharedLibrary,
  type SharedSurvey,
} from './src/sharedalbums/summary';
import { formatAge, formatBytes, formatCount, formatLastBackup } from './src/stats/format';
import { DEFAULT_GATE, NO_GATE, readConditions } from './src/sync/conditions';
import { DEFAULT_ENGINE_CONFIG, SyncEngine } from './src/sync/engine';
import { PhotoKitMediaSource } from './src/sync/media';
import { openQueueStore, type SqliteQueueStore } from './src/sync/sqliteStore';
import { HttpTransport } from './src/sync/transport';
import {
  emptyCounts,
  errorText,
  isUnauthorized,
  systemClock,
  type Progress,
  type QueueItem,
  type StateCounts,
} from './src/sync/types';

const IDLE: Progress = {
  phase: 'idle',
  activity: 'Idle',
  counts: emptyCounts(),
  retryAt: 0,
};

export default function App() {
  const [permission, requestPermission] = usePermissions();
  const [config, setConfig] = useState<Config>(loadConfig);
  const [store, setStore] = useState<SqliteQueueStore | null>(null);
  const [storeError, setStoreError] = useState<string | null>(null);
  const [server, setServer] = useState<ServerResolution | null>(null);
  const [resolving, setResolving] = useState(false);
  const [progress, setProgress] = useState<Progress>(IDLE);
  const [failed, setFailed] = useState<QueueItem[]>([]);
  const [logLines, setLogLines] = useState<string[]>([]);
  const [runError, setRunError] = useState<string | null>(null);
  /** Why a backup is being held back — charging, Wi-Fi — or null. */
  const [heldBecause, setHeldBecause] = useState<string | null>(null);
  const [running, setRunning] = useState(false);
  const [credential, setCredential] = useState<Credential | null>(null);
  const [credentialChecked, setCredentialChecked] = useState(false);
  const [pairingCode, setPairingCode] = useState('');
  const [deviceName, setDeviceName] = useState('iPhone');
  const [pairing, setPairing] = useState(false);
  const [pairError, setPairError] = useState<string | null>(null);
  const [galleryCheck, setGalleryCheck] = useState<CheckResult | null>(null);
  const [checkingGallery, setCheckingGallery] = useState(false);
  const [library, setLibrary] = useState<SharedLibrary | null>(null);
  const [surveying, setSurveying] = useState(false);
  const [surveyError, setSurveyError] = useState<string | null>(null);
  /** The shared albums ticked for backup. Empty until somebody ticks one. */
  const [chosenIds, setChosenIds] = useState<string[]>(loadSelection);
  const [sampleSize, setSampleSize] = useState(SAMPLE_SIZE);
  const [fetchRun, setFetchRun] = useState<FetchRun | null>(null);
  const [fetchingSamples, setFetchingSamples] = useState(false);
  const [forgotten, setForgotten] = useState<number | null>(null);
  const [provenance, setProvenance] = useState<SharedProvenance | null>(null);
  const [provenanceError, setProvenanceError] = useState<string | null>(null);
  /** Seeded from the cache so the card has numbers before the first fetch lands. */
  const [stats, setStats] = useState<CachedStats | null>(loadCachedStats);
  /** True when the last refresh failed and what is on screen is the cache. */
  const [statsStale, setStatsStale] = useState(false);
  const [loadingStats, setLoadingStats] = useState(false);

  // The transport reads both of these on every request, so a re-resolved address
  // or a fresh pairing takes effect without rebuilding the engine mid-run.
  const serverUrl = useRef<string>(config.serverUrl);
  const deviceToken = useRef<string | null>(null);
  const engine = useRef<SyncEngine | null>(null);

  // A ref rather than state, because the run reads it from inside a closure that
  // was built when the button was pressed. State captured there is frozen at the
  // value it had then, which for a stop flag is permanently false.
  const cancelFetch = useRef(false);

  // Built once and kept, for the same reason the transport is: it reads the
  // address and the token per request, so it survives both changing under it.
  const gallery = useRef(
    new GalleryClient(
      () => serverUrl.current,
      () => deviceToken.current
    )
  );

  useEffect(() => {
    loadCredential()
      .then((found) => {
        setCredential(found);
        deviceToken.current = found?.token ?? null;
      })
      .finally(() => setCredentialChecked(true));
  }, []);

  const granted = permission?.granted ?? false;
  const limitedAccess = permission?.accessPrivileges === 'limited';

  useEffect(() => {
    if (!permission) return;
    if (!permission.granted && permission.canAskAgain) requestPermission();
  }, [permission, requestPermission]);

  useEffect(() => {
    openQueueStore()
      .then(setStore)
      .catch((e) => setStoreError(errorText(e)));
  }, []);

  const refreshQueue = useCallback(
    async (queue: SqliteQueueStore) => {
      const [counts, failedItems] = await Promise.all([queue.counts(), queue.failed(20)]);
      setProgress((current) => ({ ...current, counts }));
      setFailed(failedItems);
    },
    []
  );

  useEffect(() => {
    if (!store) return;
    void refreshQueue(store);
  }, [store, refreshQueue]);

  /**
   * Re-reads what the archive holds.
   *
   * A failure is deliberately not an error on screen. These numbers describe
   * photos that are archived whether or not the phone can reach the server to
   * ask about them, so an unreachable server marks the card stale and leaves the
   * last known figures up rather than blanking it.
   */
  const refreshStats = useCallback(async () => {
    if (!serverUrl.current || !deviceToken.current) return;
    setLoadingStats(true);
    try {
      const entry = { fetchedAt: Date.now(), stats: await gallery.current.stats() };
      setStats(entry);
      saveCachedStats(entry);
      setStatsStale(false);
    } catch {
      setStatsStale(true);
    } finally {
      setLoadingStats(false);
    }
  }, []);

  // Once, as soon as there is both an address and a token. On a fresh install
  // that is not at launch at all — the gate stays shut until a pairing succeeds,
  // and the fetch happens then.
  const statsFetched = useRef(false);
  useEffect(() => {
    if (statsFetched.current || !credential || !server?.url) return;
    statsFetched.current = true;
    void refreshStats();
  }, [credential, server, refreshStats]);

  const findServer = useCallback(async () => {
    setResolving(true);
    try {
      const resolution = await resolveServer({
        rememberedUrl: config.lastServerUrl,
        manualUrl: config.serverUrl.trim() || null,
      });
      setServer(resolution);
      if (!resolution.url) return;

      serverUrl.current = resolution.url;
      if (resolution.url !== config.lastServerUrl) {
        const next = { ...config, lastServerUrl: resolution.url };
        setConfig(next);
        saveConfig(next);
      }
    } finally {
      setResolving(false);
    }
  }, [config]);

  // One automatic scan on launch; after that it is a button, so a slow or denied
  // scan never blocks the screen repeatedly.
  const scannedOnce = useRef(false);
  useEffect(() => {
    if (scannedOnce.current) return;
    scannedOnce.current = true;
    void findServer();
  }, [findServer]);

  const submitPairing = useCallback(async () => {
    if (!serverUrl.current) return;
    setPairing(true);
    setPairError(null);
    try {
      const paired = await pair({
        serverUrl: serverUrl.current,
        code: pairingCode,
        deviceName,
      });
      setCredential(paired);
      deviceToken.current = paired.token;
      setPairingCode('');
      setRunError(null);
    } catch (e) {
      setPairError(errorText(e));
    } finally {
      setPairing(false);
    }
  }, [pairingCode, deviceName]);

  const unpair = useCallback(async () => {
    await clearCredential();
    setCredential(null);
    deviceToken.current = null;
    setGalleryCheck(null);

    // The cached figures belong to the device that was just forgotten. Left up,
    // they would credit whatever phone pairs next with somebody else's backup.
    clearCachedStats();
    setStats(null);
    setStatsStale(false);
    statsFetched.current = false;
  }, []);

  /**
   * Proves this phone can read the archive, and that an unpaired one cannot.
   *
   * Scaffolding for the in-app gallery rather than the gallery itself: the
   * dashboard grows in the browser first and gets ported here, so what needs to
   * exist now is the connection it will be ported onto, with evidence that it
   * works and is closed to everyone else.
   *
   * Deliberately free of side effects, unlike the sync engine's handling of the
   * same 401. A diagnostic that threw away the keychain entry when it did not
   * like an answer would be a poor diagnostic; this reports and leaves the
   * Forget button to the person reading it.
   */
  const runGalleryCheck = useCallback(async () => {
    setCheckingGallery(true);
    try {
      setGalleryCheck(await checkGalleryAccess(gallery.current));
    } finally {
      setCheckingGallery(false);
    }
  }, []);

  /**
   * Looks at the iCloud Shared Albums on this phone without touching one.
   *
   * Nothing here feeds the backup — see src/sharedalbums/survey.ts for why it
   * exists at all. It is here rather than behind a developer flag because the
   * thing it measures is on this phone and nowhere else, and the answer decides
   * whether shared albums are worth teaching the queue about.
   */
  const runSurvey = useCallback(async () => {
    setSurveying(true);
    setSurveyError(null);
    // The old readings describe a library that has just been re-read. Leaving
    // them beside fresh counts would invite reading one against the other.
    setFetchRun(null);
    try {
      setLibrary(await surveySharedAlbums());
    } catch (e) {
      setSurveyError(errorText(e));
    } finally {
      setSurveying(false);
    }
  }, []);

  /**
   * The albums the backup will take, which is exactly the ones that are ticked.
   *
   * Nothing is in by default. See src/sharedalbums/selection.ts: while this was
   * a survey an unanswered question could mean "all of them" for free, and now
   * that ticking an album uploads it, it cannot.
   */
  const chosenAlbums = useMemo(() => {
    if (!library) return [];
    const wanted = new Set(chosenIds);
    return library.albums.filter((album) => wanted.has(album.localId));
  }, [library, chosenIds]);

  // Two summaries of the same phone. The first is the picker's rows, which have
  // to list albums that are not selected in order for them to be selectable; the
  // second is everything else on screen, which is about what was chosen.
  const everyAlbum = useMemo(() => (library ? summarize(library.albums) : null), [library]);
  const chosen = useMemo(() => summarize(chosenAlbums), [chosenAlbums]);
  const sample = useMemo(
    () => pickSample(assetsOf(chosenAlbums), sampleSize),
    [chosenAlbums, sampleSize]
  );

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
   * Asks one photograph what this iOS will say about who shared it.
   *
   * Here rather than in a test because the answer is a property of the phone
   * this is running on. The contributor is read from keys Apple does not
   * document, and a photograph reporting none is ambiguous between having none
   * and this build not knowing what the field is called; only the phone can
   * settle that. See sharedProvenance() in the native module.
   */
  const probeProvenance = useCallback(async () => {
    setProvenanceError(null);
    const subject = sample[0];
    if (!subject) {
      setProvenanceError('Tick an album with something in it first.');
      return;
    }
    try {
      const found = await photoKitSharedProvenance(subject.localId);
      setProvenance(found);
      if (found === null) {
        setProvenanceError('This dev client has no provenance diagnostic in it.');
      }
    } catch (error) {
      setProvenanceError(errorText(error));
    }
  }, [sample]);

  const chooseAlbums = useCallback((ids: string[]) => {
    saveSelection(ids);
    setChosenIds(ids);
  }, []);

  const toggleAlbum = useCallback(
    (localId: string) => {
      chooseAlbums(
        chosenIds.includes(localId)
          ? chosenIds.filter((id) => id !== localId)
          : [...chosenIds, localId]
      );
    },
    [chosenIds, chooseAlbums]
  );

  /**
   * Fetches the sample from iCloud, reporting itself all the way through.
   *
   * The run drives its own pacing and retries — see src/sharedalbums/run.ts.
   * This owns the two things a screen owns: where the progress goes, and the
   * flag that stops it.
   */
  const runSamples = useCallback(async () => {
    if (sample.length === 0 || fetchingSamples) return;
    cancelFetch.current = false;
    setFetchingSamples(true);
    setFetchRun(null);
    try {
      await fetchSharedAssets(sample, setFetchRun, () => cancelFetch.current);
    } catch (e) {
      setSurveyError(errorText(e));
    } finally {
      setFetchingSamples(false);
    }
  }, [sample, fetchingSamples]);

  const stopSamples = useCallback(() => {
    cancelFetch.current = true;
  }, []);

  const start = useCallback(
    async (override = false) => {
      if (!store || !credential || engine.current?.isRunning) return;
      setRunError(null);

      // Risk 6: 100GB heats the phone and drains the battery. Bulk work waits
      // for a charger and Wi-Fi unless the user explicitly says otherwise, and
      // the reason it is waiting is on screen rather than implied by nothing
      // happening.
      const conditions = await readConditions(override ? NO_GATE : DEFAULT_GATE);
      setHeldBecause(conditions.blockedBy);
      if (conditions.blockedBy) return;

      setRunning(true);
      const log = (line: string) => setLogLines((lines) => [line, ...lines].slice(0, 20));

      const instance = new SyncEngine(
        {
          store,
          // The ticked albums, read at the moment the run starts rather than
          // held by the source: enumeration happens once per run, and a run
          // already going should finish the set it was started on.
          media: new PhotoKitMediaSource({ albumIds: chosenIds }),
          transport: new HttpTransport(() => serverUrl.current, () => deviceToken.current, log),
          clock: systemClock,
          onProgress: setProgress,
          onLog: log,
        },
        { ...DEFAULT_ENGINE_CONFIG, deviceId: credential.deviceId, maxItems: config.maxItems }
      );
      engine.current = instance;

      try {
        await instance.run();
      } catch (e) {
        // A refused device is not a run that went wrong, it is a pairing that
        // has to be redone. Dropping the dead credential is what puts the
        // pairing form back on screen instead of leaving a Start button that
        // cannot work.
        if (isUnauthorized(e)) {
          await unpair();
          setRunError(
            `${errorText(e)} — pair this phone again with a fresh code from \`photobackup pair\`.`
          );
        } else {
          setRunError(errorText(e));
        }
      } finally {
        setRunning(false);
        await refreshQueue(store);
        // The moment the archived count matters most is the moment a run ends,
        // and it is the one moment it is guaranteed to have changed.
        await refreshStats();
      }
    },
    [store, credential, config.maxItems, chosenIds, refreshQueue, refreshStats, unpair]
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
    setLogLines((lines) =>
      [`reopened ${reopened} finished item(s) to re-check against the archive`, ...lines].slice(0, 20)
    );
  }, [store, refreshQueue]);

  const onChangeServer = useCallback(
    (value: string) => {
      const next = { ...config, serverUrl: value };
      setConfig(next);
      saveConfig(next);
    },
    [config]
  );

  const onChangeMaxItems = useCallback(
    (value: string) => {
      const parsed = Number.parseInt(value.replace(/[^0-9]/g, ''), 10);
      const next = { ...config, maxItems: Number.isFinite(parsed) ? parsed : DEFAULT_MAX_ITEMS };
      setConfig(next);
      saveConfig(next);
    },
    [config]
  );

  const canStart =
    Boolean(store) && granted && Boolean(serverUrl.current) && Boolean(credential) && !running;

  return (
    <SafeAreaView style={styles.screen}>
      <StatusBar style="light" />
      <ScrollView contentContainerStyle={styles.content}>
        <Text style={styles.title}>photobackup</Text>

        <Section title="Server">
          <Text style={server?.url ? styles.good : styles.muted}>
            {resolving ? 'looking for a server…' : (server?.note ?? 'not checked')}
          </Text>
          {server?.emptyScan && (
            <Text style={styles.warning}>
              An empty scan means either photod is not running or Local Network access was denied.
              Check Settings › photobackup › Local Network.
            </Text>
          )}
          <View style={styles.row}>
            <TextInput
              style={styles.input}
              value={config.serverUrl}
              onChangeText={onChangeServer}
              autoCapitalize="none"
              autoCorrect={false}
              keyboardType="url"
              placeholder="https://10.0.0.2:8787"
              placeholderTextColor="#666"
            />
            <Pressable style={styles.button} onPress={findServer} disabled={resolving}>
              <Text style={styles.buttonText}>Find</Text>
            </Pressable>
          </View>
        </Section>

        <Section title="Pairing">
          {!credentialChecked ? (
            <Text style={styles.muted}>checking the keychain…</Text>
          ) : credential ? (
            <>
              <Text style={styles.good}>
                Paired with {credential.serverName} as {credential.deviceId.slice(0, 8)}
              </Text>
              <Text style={styles.muted}>
                The token lives in the keychain and never expires. Unpairing here only
                forgets it on this phone — to stop it working at all, run{' '}
                <Text style={styles.code}>photobackup devices --revoke</Text> on the server.
              </Text>
              <Pressable style={styles.button} onPress={unpair} disabled={running}>
                <Text style={styles.buttonText}>Forget this pairing</Text>
              </Pressable>
            </>
          ) : (
            <>
              <Text style={styles.muted}>
                Run <Text style={styles.code}>photobackup pair</Text> on the server and type
                the eight-character code. It is good for ten minutes and works once.
              </Text>
              <View style={styles.row}>
                <TextInput
                  style={[styles.input, styles.codeInput]}
                  value={pairingCode}
                  onChangeText={setPairingCode}
                  autoCapitalize="characters"
                  autoCorrect={false}
                  autoComplete="off"
                  placeholder="ABCD-EFGH"
                  placeholderTextColor="#666"
                  editable={!pairing}
                  maxLength={9}
                />
                <Pressable
                  style={[styles.button, (pairing || !serverUrl.current) && styles.buttonDisabled]}
                  onPress={() => void submitPairing()}
                  disabled={pairing || !serverUrl.current}
                >
                  <Text style={styles.buttonText}>{pairing ? 'Pairing…' : 'Pair'}</Text>
                </Pressable>
              </View>
              <View style={styles.row}>
                <Text style={styles.label}>This device's name</Text>
                <TextInput
                  style={styles.nameInput}
                  value={deviceName}
                  onChangeText={setDeviceName}
                  autoCorrect={false}
                  editable={!pairing}
                />
              </View>
              {pairError && <Text style={styles.warning}>{pairError}</Text>}
              <Text style={styles.muted}>
                Pairing needs photod's certificate installed and trusted first, under
                Settings › General › About › Certificate Trust Settings. Without it every
                attempt fails as though the server were unreachable, because iOS reports a
                rejected certificate and a dead host identically.
              </Text>
            </>
          )}
        </Section>

        <Section title="Gallery access">
          <Text style={styles.muted}>
            The archive's read path now wants the same device token uploads carry, so
            browsing it from this phone needs no second credential. This checks that end
            to end before any of the gallery is built on it.
          </Text>
          <Pressable
            style={[styles.button, (!credential || checkingGallery) && styles.buttonDisabled]}
            onPress={() => void runGalleryCheck()}
            disabled={!credential || checkingGallery}
          >
            <Text style={styles.buttonText}>
              {checkingGallery ? 'Checking…' : 'Check gallery access'}
            </Text>
          </Pressable>
          {!credential && <Text style={styles.muted}>Pair this phone first.</Text>}

          {galleryCheck?.steps.map((step) => (
            <View key={step.label} style={styles.checkRow}>
              <Text style={step.ok ? styles.good : styles.bad}>{step.ok ? '✓' : '✗'}</Text>
              <View style={styles.grow}>
                <Text style={styles.checkLabel}>{step.label}</Text>
                <Text style={styles.muted}>{step.detail}</Text>
              </View>
            </View>
          ))}

          {galleryCheck?.unauthorized && (
            <Text style={styles.warning}>
              The server refused this phone's token. It has probably been revoked with{' '}
              <Text style={styles.code}>photobackup devices --revoke</Text> — forget the
              pairing above and pair again with a fresh code.
            </Text>
          )}
          {galleryCheck && !galleryCheck.ok && !galleryCheck.unauthorized && (
            <Text style={styles.warning}>
              Something in the read path is not as it should be. A failure on the last step
              in particular means photod is answering callers that hold no token.
            </Text>
          )}
        </Section>

        <Section title="Shared albums">
          <Text style={styles.muted}>
            iCloud Shared Albums live in collections of their own, outside the library the
            backup enumerates. Tick the ones to archive: their photos join the ordinary
            backup, filed under the album&rsquo;s name, with whoever added each one recorded
            beside it.
          </Text>
          <Pressable
            style={[styles.button, (!granted || surveying) && styles.buttonDisabled]}
            onPress={() => void runSurvey()}
            disabled={!granted || surveying}
          >
            <Text style={styles.buttonText}>
              {surveying ? 'Surveying…' : 'Survey shared albums'}
            </Text>
          </Pressable>
          {surveyError && <Text style={styles.warning}>{surveyError}</Text>}
          {limitedAccess && (
            <Text style={styles.warning}>
              Limited access hides shared albums entirely, so a survey run now will find none
              whether or not there are any.
            </Text>
          )}

          {library?.supported && library.albums.length > 0 && !canDownloadShared && (
            <Text style={styles.warning}>
              This dev client can list shared albums but not fetch them, so ticking one would
              queue photos that every run then fails on. Rebuild it with{' '}
              <Text style={styles.code}>pnpm ios</Text> first.
            </Text>
          )}

          {library && !library.supported && (
            <Text style={styles.warning}>
              This dev client has no shared-album enumerator in it. Rebuild it with{' '}
              <Text style={styles.code}>pnpm ios</Text> and run the survey again.
            </Text>
          )}

          {library?.supported && library.albums.length === 0 && (
            <Text style={styles.muted}>
              No shared albums on this phone — so there is nothing missing from the backup,
              and nothing here to decide about. Shared Albums can also be switched off
              entirely under Settings › Photos, which looks exactly like this.
            </Text>
          )}

          {chosenIds.length > 0 && !library && (
            <Text style={styles.muted}>
              {formatCount(chosenIds.length)} album(s) are ticked from a previous session and
              will be backed up by the next run. Survey to see them.
            </Text>
          )}

          {library?.supported && everyAlbum && library.albums.length > 0 && (
            <>
              <AlbumPicker
                albums={everyAlbum.albums}
                chosen={chosenIds}
                onToggle={toggleAlbum}
                onChoose={chooseAlbums}
              />
              <SharedSurveyReport survey={chosen} />
              <FetchPanel
                sample={sample}
                size={sampleSize}
                onSize={setSampleSize}
                run={fetchRun}
                fetching={fetchingSamples}
                onFetch={() => void runSamples()}
                onStop={stopSamples}
              />
              <SharedRepairPanel
                onForget={() => void forgetShared()}
                forgotten={forgotten}
                disabled={!store}
                onProbe={() => void probeProvenance()}
                provenance={provenance}
                provenanceError={provenanceError}
              />
            </>
          )}
        </Section>

        {!granted && (
          <Text style={styles.warning}>
            Photo library access is required. Grant it in Settings to back anything up.
          </Text>
        )}
        {limitedAccess && (
          <Text style={styles.warning}>
            Limited access is on, so only hand-picked photos are visible. Choose “All Photos” in
            Settings for a real backup.
          </Text>
        )}
        {storeError && <Text style={styles.warning}>Queue unavailable: {storeError}</Text>}
        {runError && <Text style={styles.warning}>Run stopped: {runError}</Text>}

        <Section title="Backup">
          <View style={styles.row}>
            <Pressable
              style={[styles.button, styles.grow, !canStart && styles.buttonDisabled]}
              onPress={() => start()}
              disabled={!canStart}
            >
              <Text style={styles.buttonText}>{running ? 'Running…' : 'Start backup'}</Text>
            </Pressable>
            <Pressable
              style={[styles.button, !running && styles.buttonDisabled]}
              onPress={pause}
              disabled={!running}
            >
              <Text style={styles.buttonText}>Pause</Text>
            </Pressable>
          </View>

          {heldBecause && !running && (
            <View style={styles.status}>
              <Text style={styles.statusText}>
                Holding off — {heldBecause}. A full backup warms the phone up and eats
                battery, so it waits for a charger and Wi-Fi.
              </Text>
              <Pressable style={styles.button} onPress={() => void start(true)}>
                <Text style={styles.buttonText}>Back up anyway</Text>
              </Pressable>
            </View>
          )}

          <View style={styles.status}>
            {running && <ActivityIndicator color="#8ab4f8" />}
            <Text style={styles.statusText}>{progress.activity}</Text>
          </View>

          <ArchiveSummary
            entry={stats}
            stale={statsStale}
            loading={loadingStats}
            paired={Boolean(credential)}
          />

          <Text style={styles.subheading}>this run</Text>
          <RunCounts counts={progress.counts} />

          <View style={styles.row}>
            <Text style={styles.label}>Newest items to consider</Text>
            <TextInput
              style={styles.numberInput}
              value={String(config.maxItems)}
              onChangeText={onChangeMaxItems}
              keyboardType="number-pad"
              editable={!running}
            />
          </View>
          <Text style={styles.muted}>0 backs up the whole library.</Text>

          <Pressable
            style={[styles.button, running && styles.buttonDisabled]}
            onPress={recheckArchive}
            disabled={running}
          >
            <Text style={styles.buttonText}>Re-check the archive</Text>
          </Pressable>
          <Text style={styles.muted}>
            Forgets what this phone believes it has already backed up, without touching a photo.
            The next run asks the archive about every item again and re-sends only what is
            genuinely missing. Use this if the archive was rebuilt, or if a photo you can see on
            this phone never appears in the gallery.
          </Text>
        </Section>

        {failed.length > 0 && (
          <Section title={`Failed (${failed.length})`}>
            <Pressable style={styles.button} onPress={retryFailed} disabled={running}>
              <Text style={styles.buttonText}>Retry failed items</Text>
            </Pressable>
            {failed.map((item) => (
              <View key={item.localId} style={styles.failedRow}>
                <Text style={styles.failedName}>{item.filename}</Text>
                <Text style={styles.muted}>{item.lastError ?? 'no reason recorded'}</Text>
              </View>
            ))}
          </Section>
        )}

        {logLines.length > 0 && (
          <Section title="Log">
            {logLines.map((line, index) => (
              <Text key={`${index}-${line}`} style={styles.logLine}>
                {line}
              </Text>
            ))}
          </Section>
        )}
      </ScrollView>
    </SafeAreaView>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <View style={styles.section}>
      <Text style={styles.sectionTitle}>{title}</Text>
      {children}
    </View>
  );
}

/**
 * Which shared albums are going to be imported.
 *
 * Every album is listed whether or not it is chosen, because an unlisted album
 * cannot be chosen and the point of the screen is choosing. The count beside
 * each is its own — an asset in two albums is shown under both — which is what
 * the Photos app shows and is not the number the totals below use; see
 * summarize().
 */
function AlbumPicker({
  albums,
  chosen,
  onToggle,
  onChoose,
}: {
  albums: AlbumSummary[];
  chosen: string[];
  onToggle: (localId: string) => void;
  onChoose: (ids: string[]) => void;
}) {
  const isChosen = (localId: string) => chosen.includes(localId);
  const count = albums.filter((album) => isChosen(album.localId)).length;

  return (
    <>
      <View style={styles.subheadingRow}>
        <Text style={styles.subheadingPlain}>albums to back up</Text>
        <Pressable onPress={() => onChoose(albums.map((album) => album.localId))}>
          <Text style={styles.linkText}>all</Text>
        </Pressable>
        <Pressable onPress={() => onChoose([])}>
          <Text style={styles.linkText}>none</Text>
        </Pressable>
      </View>
      <Text style={styles.muted}>
        {count} of {albums.length} chosen
        {count === 0 && ' — nothing shared is backed up until one is ticked'}
      </Text>

      {albums.map((album) => (
        <Pressable
          key={album.localId}
          style={styles.albumRow}
          onPress={() => onToggle(album.localId)}
        >
          <Text style={isChosen(album.localId) ? styles.tickOn : styles.tickOff}>
            {isChosen(album.localId) ? '◉' : '○'}
          </Text>
          <View style={styles.grow}>
            <Text style={styles.checkLabel}>{album.title ?? 'untitled album'}</Text>
            <Text style={styles.muted}>
              {formatCount(album.assets)} assets · {formatCount(album.stills)} stills ·{' '}
              {formatCount(album.videos)} videos
              {album.live > 0 && ` · ${formatCount(album.live)} live`}
            </Text>
          </View>
        </Pressable>
      ))}
    </>
  );
}

/**
 * The shared-album survey, read out.
 *
 * Written to answer one question rather than to display a structure: is Apple's
 * copy of a shared photo worth archiving? Everything on screen is arranged
 * around that, and the two findings that would change the answer — a still above
 * the documented cap, a full-size resource sitting beside the render — are
 * called out in words rather than left to be spotted in a table.
 *
 * It describes the chosen albums rather than the phone. Unticking an album has
 * to change these numbers or the picker above is decoration: the question is not
 * what iCloud is holding, it is what a backup of the chosen albums would fetch.
 *
 * The counts are deliberately flat and unformatted where they are small. This is
 * a diagnostic that will be read a handful of times by the person who wrote it
 * and then deleted or promoted; dressing it up would cost more than it is worth.
 */
function SharedSurveyReport({ survey }: { survey: SharedSurvey }) {
  if (survey.albums.length === 0) {
    return (
      <Text style={styles.muted}>
        No albums chosen, so there is nothing to survey and nothing to fetch.
      </Text>
    );
  }

  const fullSize = (survey.resourceTypes.fullSizePhoto ?? 0) + (survey.resourceTypes.fullSizeVideo ?? 0);
  const original = survey.still.overCap > 0 || fullSize > 0;

  return (
    <>
      <View style={styles.counts}>
        <Count label="albums" value={formatCount(survey.albums.length)} />
        <Count label="assets" value={formatCount(survey.assets)} tone="good" />
        <Count label="videos" value={formatCount(survey.videos)} />
      </View>
      <Text style={styles.muted}>
        {formatCount(survey.stills)} stills · {formatCount(survey.videos)} videos ·{' '}
        {formatCount(survey.live)} Live Photos
        {survey.inMultipleAlbums > 0 &&
          ` · ${formatCount(survey.inMultipleAlbums)} in more than one album`}
      </Text>
      {survey.oldest !== null && survey.newest !== null && (
        <Text style={styles.muted}>
          taken between {new Date(survey.oldest).toISOString().slice(0, 10)} and{' '}
          {new Date(survey.newest).toISOString().slice(0, 10)}
        </Text>
      )}

      <Text style={styles.subheading}>what sharing cost</Text>
      <Text style={styles.muted}>
        stills — longest edge {survey.still.maxLongEdge ?? '—'} px, {survey.still.atMax} of{' '}
        {survey.stills} sitting exactly there
      </Text>
      <Text style={styles.muted}>
        A shared still reports one pixel more than it downloads: PhotoKit says 2049 px and
        the resource arrives at 2048. The count above is the honest signal that a cap is
        being enforced — not the handful of pixels either side of it.
      </Text>
      <Text style={styles.muted}>
        videos — longest edge {survey.video.maxLongEdge ?? '—'} px, longest clip{' '}
        {formatDuration(survey.longestVideoSeconds)}
      </Text>
      {original ? (
        <Text style={styles.good}>
          {survey.still.overCap > 0
            ? `${formatCount(survey.still.overCap)} stills are above the ${STILL_LONG_EDGE_CAP}px cap`
            : `${formatCount(fullSize)} assets carry a full-size resource`}{' '}
          — Apple is not downscaling everything here, so what a backup fetched may be the
          original after all. Worth looking at one by hand before deciding.
        </Text>
      ) : (
        <Text style={styles.muted}>
          Nothing exceeds Apple&apos;s documented caps ({STILL_LONG_EDGE_CAP}px on a photo,{' '}
          {formatDuration(VIDEO_SECONDS_CAP)} on a video) and no full-size resource exists, so
          every one of these is Apple&apos;s re-encode — a JPEG, whatever the original was
          shot as. Backing them up archives the downscale, which is the only copy that
          exists for anything you did not share yourself.
        </Text>
      )}
      <Text style={styles.muted}>
        resources — {inventory(survey.resourceTypes)}
      </Text>
      <Text style={styles.muted}>sources — {inventory(survey.sourceTypes)}</Text>
    </>
  );
}

/**
 * The fetch: how much of it to do, doing it, and what came back.
 *
 * The size is a control rather than a constant because the two things worth
 * learning here need different amounts of it. Three assets show what a shared
 * photo weighs and how long one takes. Nothing under a few hundred shows what
 * iCloud does to a phone that asks for hundreds in a row, and that is the
 * question a backup depends on the answer to.
 */
/**
 * The two things to do when a shared photograph reached the archive with
 * something missing from it.
 *
 * Both are about metadata rather than bytes, and neither re-fetches anything
 * from iCloud.
 */
function SharedRepairPanel({
  onForget,
  forgotten,
  disabled,
  onProbe,
  provenance,
  provenanceError,
}: {
  onForget: () => void;
  forgotten: number | null;
  disabled: boolean;
  onProbe: () => void;
  provenance: SharedProvenance | null;
  provenanceError: string | null;
}) {
  return (
    <>
      <Text style={styles.subheading}>fixing what was already archived</Text>
      <Text style={styles.muted}>
        A shared photograph records its album and who added it at the moment it is queued,
        so anything archived by an earlier build kept neither. This forgets what the queue
        remembers about shared items; the next run offers them again, the server answers
        &ldquo;already have it&rdquo;, and each one is described on the way past. No bytes
        are fetched and nothing is uploaded twice.
      </Text>
      <Pressable
        style={[styles.button, disabled && styles.buttonDisabled]}
        onPress={onForget}
        disabled={disabled}
      >
        <Text style={styles.buttonText}>Re-send shared album details</Text>
      </Pressable>
      {forgotten !== null && (
        <Text style={styles.muted}>
          {forgotten === 0
            ? 'Nothing shared was in the queue. Survey and tick an album, then run a backup.'
            : `${formatCount(forgotten)} shared item(s) forgotten. Run a backup to re-send them.`}
        </Text>
      )}

      <Text style={styles.muted}>
        The contributor is read from PhotoKit properties Apple does not document, so an
        empty &ldquo;added by&rdquo; can mean the photograph has no contributor or that this
        iOS calls that field something else. This asks one photograph directly.
      </Text>
      <Pressable style={styles.button} onPress={onProbe}>
        <Text style={styles.buttonText}>Read provenance for one photo</Text>
      </Pressable>
      {provenanceError && <Text style={styles.warning}>{provenanceError}</Text>}
      {provenance && (
        <View style={styles.block}>
          <Text style={styles.mono}>
            {provenance.class} · {provenance.sourceTypes.names.join(', ') || 'no source type'}
          </Text>
          <Text style={styles.mono}>
            contributor: {provenance.contributor?.displayName ?? 'none'}
          </Text>
          <Text style={styles.mono}>looked under: {provenance.contributorKeys.join(', ')}</Text>
          {provenance.albums.map((album, index) => (
            <Text key={index} style={styles.mono}>
              album {album.title ?? 'untitled'} · owner {album.contributor?.displayName ?? 'none'}
            </Text>
          ))}
          {provenance.properties.map((line, index) => (
            <Text key={`p${index}`} style={styles.mono}>
              {line}
            </Text>
          ))}
        </View>
      )}
    </>
  );
}

function FetchPanel({
  sample,
  size,
  onSize,
  run,
  fetching,
  onFetch,
  onStop,
}: {
  sample: SharedAsset[];
  size: number;
  onSize: (size: number) => void;
  run: FetchRun | null;
  fetching: boolean;
  onFetch: () => void;
  onStop: () => void;
}) {
  return (
    <>
      <Text style={styles.subheading}>fetching from iCloud</Text>
      <Text style={styles.muted}>
        A shared asset has no original on the disk, so reading one means asking iCloud for it.
        This fetches them one at a time and keeps none of the bytes — it reports the size,
        how long each took, and how it fails. It slows down when iCloud starts refusing and
        stops on its own if it keeps doing so.
      </Text>

      <View style={styles.row}>
        {SAMPLE_SIZES.map((option) => (
          <Pressable
            key={option}
            style={[styles.chip, option === size && styles.chipOn]}
            onPress={() => onSize(option)}
            disabled={fetching}
          >
            <Text style={option === size ? styles.chipTextOn : styles.chipText}>{option}</Text>
          </Pressable>
        ))}
      </View>

      <View style={styles.row}>
        <Pressable
          style={[styles.button, styles.grow, (fetching || sample.length === 0) && styles.buttonDisabled]}
          onPress={onFetch}
          disabled={fetching || sample.length === 0}
        >
          <Text style={styles.buttonText}>
            {fetching ? 'Fetching…' : `Fetch ${sample.length} from iCloud`}
          </Text>
        </Pressable>
        {fetching && (
          <Pressable style={styles.button} onPress={onStop}>
            <Text style={styles.buttonText}>Stop</Text>
          </Pressable>
        )}
      </View>

      {run && <FetchProgress run={run} />}
      {run && run.results.length > 0 && <ResultRows results={run.results} />}
    </>
  );
}

/** Failures shown at most, before the list stops being a list. */
const SHOWN_FAILURES = 40;

/** Successes shown, counting back from the most recent. */
const SHOWN_SUCCESSES = 10;

/**
 * The results, thinned to what a person would actually read.
 *
 * Five hundred rows in a ScrollView that redraws several times a second is a
 * stutter, and four hundred and ninety of them say the same thing. Failures are
 * kept whatever their age, because they are the reason this screen exists; the
 * successes are kept only while they are recent, because their value is showing
 * that the run is still working and yesterday's does not.
 */
function ResultRows({ results }: { results: SampleRead[] }) {
  const recent = new Set(results.filter((r) => r.error === null).slice(-SHOWN_SUCCESSES));
  const failures = results.filter((r) => r.error !== null);
  const kept = new Set(failures.slice(-SHOWN_FAILURES));
  // Filtered in place rather than concatenated, so the rows stay in the order
  // they were fetched in and a failure keeps the successes around it.
  const shown = results.filter((r) => recent.has(r) || kept.has(r));

  return (
    <>
      {shown.length < results.length && (
        <Text style={styles.muted}>
          showing {shown.length} of {results.length} — every failure, and the last{' '}
          {SHOWN_SUCCESSES} that worked
        </Text>
      )}
      {shown.map((sampleRead) => (
        <SampleRow key={sampleRead.asset.localId} sample={sampleRead} />
      ))}
    </>
  );
}

/**
 * How far the run has got.
 *
 * The bar counts finished assets plus however far into the current one iCloud
 * says it is, which is the only way it moves at all during a video: one of those
 * is six seconds on its own, and a bar that advances once every six seconds is a
 * bar that looks broken. On a build with no progress event in it the fraction is
 * always zero and the bar advances per asset, which is the honest version of the
 * same picture.
 */
function FetchProgress({ run }: { run: FetchRun }) {
  const fraction = run.total === 0 ? 0 : (run.done + (run.current?.fraction ?? 0)) / run.total;

  return (
    <>
      <View style={styles.progressTrack}>
        <View style={[styles.progressFill, { width: `${Math.min(100, fraction * 100)}%` }]} />
      </View>
      <Text style={styles.muted}>
        {run.done} of {run.total} · {formatBytes(run.bytes)}
        {run.failed > 0 && ` · ${run.failed} failed`}
      </Text>

      {run.current && (
        <Text style={styles.logLine}>
          {run.current.asset.filename ?? run.current.asset.localId}
          {run.current.attempt > 1 && ` · attempt ${run.current.attempt}`}
          {run.current.bytes > 0 && ` · ${formatBytes(run.current.bytes)}`}
          {run.current.fraction > 0 && ` · ${Math.round(run.current.fraction * 100)}%`}
        </Text>
      )}
      {/* The gap between two healthy fetches is a fraction of a second, and a
          line that appears and vanishes at that rate is worse than none. This
          shows only the waits that are a backing-off rather than a pause. */}
      {run.waitingMs >= 1_000 && (
        <Text style={styles.muted}>
          waiting {(run.waitingMs / 1000).toFixed(1)}s before the next attempt
        </Text>
      )}
      {run.stoppedBecause && <Text style={styles.warning}>{run.stoppedBecause}</Text>}
    </>
  );
}

function SampleRow({ sample }: { sample: SampleRead }) {
  const { asset, read, error, attempts } = sample;
  const rate = read ? throughputMbPerSecond(read.bytes, read.elapsedMs) : null;

  return (
    <View style={styles.failedRow}>
      <Text style={styles.checkLabel}>
        {error ? '✗' : '✓'} {asset.filename ?? asset.localId}
        {attempts > 1 && <Text style={styles.muted}> after {attempts} attempts</Text>}
      </Text>
      {read ? (
        <Text style={styles.logLine}>
          {read.resourceType} · {asset.pixelWidth}×{asset.pixelHeight}
          {asset.kind === 'video' && ` · ${formatDuration(asset.durationSeconds)}`} ·{' '}
          {formatBytes(read.bytes)} · {(read.elapsedMs / 1000).toFixed(1)}s
          {rate !== null && ` · ${rate.toFixed(1)} MB/s`}
        </Text>
      ) : (
        <Text style={styles.bad}>{error}</Text>
      )}
      {read?.originalFilename && read.originalFilename !== asset.filename && (
        <Text style={styles.muted}>the camera called it {read.originalFilename}</Text>
      )}
    </View>
  );
}

/** "photo ×812, adjustmentData ×4", commonest first. */
function inventory(counts: Record<string, number>): string {
  const entries = Object.entries(counts).sort(([, a], [, b]) => b - a);
  if (entries.length === 0) return 'none reported';
  return entries.map(([name, count]) => `${name} ×${formatCount(count)}`).join(', ');
}

/**
 * What is actually archived, as photod reports it.
 *
 * These numbers used to be counted from the local queue, which made them a
 * record of what this app had done rather than of what is stored: reinstalling
 * it, or losing the SQLite file, showed zero archived against a library that was
 * entirely backed up. The archive is the only thing that knows what the archive
 * holds, so it is asked.
 *
 * The cost is that they arrive over the network and can be missing. An
 * unreachable server shows the last known figures marked stale rather than
 * dashes, because the photos are archived either way and a blank card would
 * imply otherwise.
 */
function ArchiveSummary({
  entry,
  stale,
  loading,
  paired,
}: {
  entry: CachedStats | null;
  stale: boolean;
  loading: boolean;
  paired: boolean;
}) {
  if (!paired) {
    return <Text style={styles.muted}>Pair this phone to see what the archive holds.</Text>;
  }

  if (!entry) {
    return (
      <>
        <View style={styles.counts}>
          <Count label="archived" value="—" />
          <Count label="stored" value="—" />
          <Count label="last backup" value="—" />
        </View>
        <Text style={styles.muted}>
          {loading ? 'asking the server…' : 'could not reach the server for these yet.'}
        </Text>
      </>
    );
  }

  const { device, archive } = entry.stats;
  const now = Date.now();
  return (
    <>
      <View style={styles.counts}>
        <Count label="archived" value={formatCount(device.archived)} tone="good" />
        <Count label="stored" value={formatBytes(device.bytes)} />
        <Count label="last backup" value={formatLastBackup(device.last_upload_at, now)} />
      </View>
      <Text style={styles.muted}>
        {formatCount(device.photos)} photos · {formatCount(device.videos)} videos from this phone
      </Text>
      <Text style={styles.muted}>
        The archive holds {formatCount(archive.assets)} items, {formatBytes(archive.bytes)}
        {archive.pending_jobs > 0 &&
          ` · ${formatCount(archive.pending_jobs)} thumbnails still being built`}
      </Text>
      {archive.failed_jobs > 0 && (
        <Text style={styles.warning}>
          {formatCount(archive.failed_jobs)} derivatives failed on the server. Nothing is lost —
          the originals are archived — but those tiles will not fill in.
        </Text>
      )}
      {stale && (
        <Text style={styles.warning}>
          as of {formatAge(entry.fetchedAt, now)} — could not reach the server to refresh.
        </Text>
      )}
    </>
  );
}

/**
 * The queue, which is about this run and nothing else.
 *
 * `archived` and `total` used to be here and are the server's now. What is left
 * is the part only the phone can know: what it has yet to look at, what it has
 * yet to send, and what it could not.
 */
function RunCounts({ counts }: { counts: StateCounts }) {
  const queued = counts.pending + counts.unknown + counts.hashed;
  return (
    <View style={styles.counts}>
      <Count label="queued" value={String(queued)} />
      <Count label="to send" value={String(counts.want)} />
      <Count
        label="failed"
        value={String(counts.failed)}
        tone={counts.failed > 0 ? 'bad' : undefined}
      />
    </View>
  );
}

function Count({
  label,
  value,
  tone,
}: {
  label: string;
  value: string;
  tone?: 'good' | 'bad';
}) {
  const color = tone === 'good' ? styles.good : tone === 'bad' ? styles.bad : styles.countValue;
  return (
    <View style={styles.count}>
      <Text style={[styles.countValue, color]}>{value}</Text>
      <Text style={styles.countLabel}>{label}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: '#111' },
  content: { paddingHorizontal: 16, paddingBottom: 40 },
  title: { color: '#eee', fontSize: 20, fontWeight: '600', marginTop: 8, marginBottom: 4 },
  section: {
    backgroundColor: '#1a1a1a',
    borderRadius: 10,
    padding: 12,
    marginTop: 12,
    gap: 8,
  },
  sectionTitle: { color: '#eee', fontSize: 13, fontWeight: '600', textTransform: 'uppercase' },
  row: { flexDirection: 'row', gap: 8, alignItems: 'center' },
  grow: { flex: 1 },
  input: {
    flex: 1,
    backgroundColor: '#242424',
    color: '#eee',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 10,
  },
  codeInput: {
    fontFamily: 'Menlo',
    letterSpacing: 2,
  },
  nameInput: {
    backgroundColor: '#242424',
    color: '#eee',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 8,
    minWidth: 140,
  },
  code: { fontFamily: 'Menlo', color: '#aaa' },
  mono: { fontFamily: 'Menlo', color: '#aaa', fontSize: 11 },
  block: {
    backgroundColor: '#161616',
    borderRadius: 8,
    padding: 10,
    marginTop: 8,
    gap: 2,
  },
  numberInput: {
    backgroundColor: '#242424',
    color: '#eee',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 8,
    minWidth: 80,
    textAlign: 'right',
  },
  label: { color: '#aaa', fontSize: 13, flex: 1 },
  button: {
    backgroundColor: '#2d4a7c',
    borderRadius: 8,
    paddingHorizontal: 16,
    paddingVertical: 11,
    alignItems: 'center',
  },
  buttonDisabled: { backgroundColor: '#2a2a2a' },
  buttonText: { color: '#eee', fontWeight: '600' },
  muted: { color: '#888', fontSize: 12 },
  good: { color: '#7ed492' },
  bad: { color: '#e2685f' },
  warning: { color: '#e8b84b', fontSize: 12, marginTop: 8 },
  status: { flexDirection: 'row', alignItems: 'center', gap: 8, minHeight: 22 },
  statusText: { color: '#8ab4f8', fontSize: 13, flex: 1 },
  counts: { flexDirection: 'row', justifyContent: 'space-between', marginTop: 4 },
  count: { alignItems: 'center' },
  countValue: { color: '#eee', fontSize: 18, fontWeight: '600' },
  subheading: {
    color: '#666',
    fontSize: 11,
    textTransform: 'uppercase',
    letterSpacing: 1,
    borderTopWidth: 1,
    borderTopColor: '#2a2a2a',
    paddingTop: 8,
    marginTop: 4,
  },
  // The subheading's rule and padding move onto the row when something sits
  // beside the heading, so the line is drawn above both rather than through one.
  subheadingRow: {
    flexDirection: 'row',
    alignItems: 'baseline',
    gap: 12,
    borderTopWidth: 1,
    borderTopColor: '#2a2a2a',
    paddingTop: 8,
    marginTop: 4,
  },
  subheadingPlain: {
    color: '#666',
    fontSize: 11,
    textTransform: 'uppercase',
    letterSpacing: 1,
    flex: 1,
  },
  linkText: { color: '#8ab4f8', fontSize: 12 },
  albumRow: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
    borderTopWidth: 1,
    borderTopColor: '#2a2a2a',
    paddingVertical: 6,
  },
  tickOn: { color: '#8ab4f8', fontSize: 16 },
  tickOff: { color: '#555', fontSize: 16 },
  chip: {
    paddingHorizontal: 12,
    paddingVertical: 6,
    borderRadius: 6,
    backgroundColor: '#2a2a2a',
  },
  chipOn: { backgroundColor: '#3a4a63' },
  chipText: { color: '#888', fontSize: 12 },
  chipTextOn: { color: '#eee', fontSize: 12, fontWeight: '600' },
  progressTrack: {
    height: 6,
    borderRadius: 3,
    backgroundColor: '#2a2a2a',
    overflow: 'hidden',
    marginTop: 8,
  },
  progressFill: { height: 6, borderRadius: 3, backgroundColor: '#8ab4f8' },
  countLabel: { color: '#777', fontSize: 11 },
  checkRow: { flexDirection: 'row', gap: 8, alignItems: 'flex-start' },
  checkLabel: { color: '#ddd', fontSize: 13 },
  failedRow: { borderTopWidth: 1, borderTopColor: '#2a2a2a', paddingTop: 6 },
  failedName: { color: '#ddd', fontSize: 13 },
  logLine: { color: '#8a8a8a', fontSize: 11, fontFamily: 'Menlo' },
});
