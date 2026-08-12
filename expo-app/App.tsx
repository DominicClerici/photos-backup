import { usePermissions } from 'expo-media-library';
import { StatusBar } from 'expo-status-bar';
import { useCallback, useEffect, useRef, useState } from 'react';
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
          media: new PhotoKitMediaSource(),
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
    [store, credential, config.maxItems, refreshQueue, refreshStats, unpair]
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
  countLabel: { color: '#777', fontSize: 11 },
  checkRow: { flexDirection: 'row', gap: 8, alignItems: 'flex-start' },
  checkLabel: { color: '#ddd', fontSize: 13 },
  failedRow: { borderTopWidth: 1, borderTopColor: '#2a2a2a', paddingTop: 6 },
  failedName: { color: '#ddd', fontSize: 13 },
  logLine: { color: '#8a8a8a', fontSize: 11, fontFamily: 'Menlo' },
});
