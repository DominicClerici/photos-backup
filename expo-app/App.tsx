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
import { clearCredential, loadCredential, pair, type Credential } from './src/pairing';
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

  // The transport reads both of these on every request, so a re-resolved address
  // or a fresh pairing takes effect without rebuilding the engine mid-run.
  const serverUrl = useRef<string>(config.serverUrl);
  const deviceToken = useRef<string | null>(null);
  const engine = useRef<SyncEngine | null>(null);

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
      }
    },
    [store, credential, config.maxItems, refreshQueue, unpair]
  );

  const pause = useCallback(() => {
    engine.current?.stop();
  }, []);

  const retryFailed = useCallback(async () => {
    if (!store) return;
    await store.resetFailed();
    await refreshQueue(store);
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

          <Counts counts={progress.counts} />

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
 * Groups the five working states into the three numbers that actually answer
 * "how far along is this": still to look at, still to send, and archived.
 */
function Counts({ counts }: { counts: StateCounts }) {
  const queued = counts.pending + counts.unknown + counts.hashed;
  const total = queued + counts.want + counts.done + counts.failed;
  return (
    <View style={styles.counts}>
      <Count label="queued" value={queued} />
      <Count label="to send" value={counts.want} />
      <Count label="archived" value={counts.done} tone="good" />
      <Count label="failed" value={counts.failed} tone={counts.failed > 0 ? 'bad' : undefined} />
      <Count label="total" value={total} />
    </View>
  );
}

function Count({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
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
  countLabel: { color: '#777', fontSize: 11 },
  failedRow: { borderTopWidth: 1, borderTopColor: '#2a2a2a', paddingTop: 6 },
  failedName: { color: '#ddd', fontSize: 13 },
  logLine: { color: '#8a8a8a', fontSize: 11, fontFamily: 'Menlo' },
});
