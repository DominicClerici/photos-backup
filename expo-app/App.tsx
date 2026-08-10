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
import { DEFAULT_GATE, NO_GATE, readConditions } from './src/sync/conditions';
import { DEFAULT_ENGINE_CONFIG, SyncEngine } from './src/sync/engine';
import { PhotoKitMediaSource } from './src/sync/media';
import { openQueueStore, type SqliteQueueStore } from './src/sync/sqliteStore';
import { HttpTransport } from './src/sync/transport';
import {
  emptyCounts,
  errorText,
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

  // The transport reads this on every request, so a re-resolved address takes
  // effect without rebuilding the engine mid-run.
  const serverUrl = useRef<string>(config.serverUrl);
  const engine = useRef<SyncEngine | null>(null);

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

  const start = useCallback(
    async (override = false) => {
      if (!store || engine.current?.isRunning) return;
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
          transport: new HttpTransport(() => serverUrl.current, log),
          clock: systemClock,
          onProgress: setProgress,
          onLog: log,
        },
        { ...DEFAULT_ENGINE_CONFIG, deviceId: config.deviceId, maxItems: config.maxItems }
      );
      engine.current = instance;

      try {
        await instance.run();
      } catch (e) {
        setRunError(errorText(e));
      } finally {
        setRunning(false);
        await refreshQueue(store);
      }
    },
    [store, config.deviceId, config.maxItems, refreshQueue]
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

  const canStart = Boolean(store) && granted && Boolean(serverUrl.current) && !running;

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
              placeholder="http://10.0.0.2:8787"
              placeholderTextColor="#666"
            />
            <Pressable style={styles.button} onPress={findServer} disabled={resolving}>
              <Text style={styles.buttonText}>Find</Text>
            </Pressable>
          </View>
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
