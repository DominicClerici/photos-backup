import { StatusBar } from 'expo-status-bar';
import { usePermissions } from 'expo-media-library';
import { useCallback, useEffect, useState } from 'react';
import {
  ActivityIndicator,
  FlatList,
  Image,
  Pressable,
  SafeAreaView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';

import { loadConfig, saveConfig } from './src/config';
import { recentPhotos, thumbnailUri, type PickerItem } from './src/library';
import { errorText, uploadAsset, type UploadResult, type UploadStage } from './src/upload';

const GRID_COLUMNS = 3;
const PHOTO_LIMIT = 30;

/**
 * Resolves its own URI on mount rather than resolving the whole page up front,
 * so a slow PhotoKit lookup delays one tile instead of the entire grid.
 */
function Thumbnail({ id }: { id: string }) {
  const [uri, setUri] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    thumbnailUri(id)
      .then((resolved) => {
        if (active) setUri(resolved);
      })
      .catch(() => {});
    return () => {
      active = false;
    };
  }, [id]);

  if (!uri) return <View style={styles.thumbnail} />;
  return <Image source={{ uri }} style={styles.thumbnail} />;
}

const STAGE_LABEL: Record<UploadStage, string> = {
  resolving: 'Resolving original…',
  hashing: 'Hashing — the app will freeze for a moment',
  uploading: 'Uploading…',
  done: 'Done',
};

export default function App() {
  const [permission, requestPermission] = usePermissions();
  const [config, setConfig] = useState(() => loadConfig());
  const [photos, setPhotos] = useState<PickerItem[]>([]);
  const [libraryError, setLibraryError] = useState<string | null>(null);
  const [health, setHealth] = useState<string>('not checked');
  const [stage, setStage] = useState<UploadStage | null>(null);
  const [result, setResult] = useState<UploadResult | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  const granted = permission?.granted ?? false;
  const limitedAccess = permission?.accessPrivileges === 'limited';

  useEffect(() => {
    if (!permission) return;
    if (!permission.granted && permission.canAskAgain) requestPermission();
  }, [permission, requestPermission]);

  useEffect(() => {
    if (!granted) return;
    recentPhotos(PHOTO_LIMIT)
      .then((items) => {
        setPhotos(items);
        setLibraryError(null);
      })
      .catch((e) => setLibraryError(errorText(e)));
  }, [granted]);

  const checkHealth = useCallback(async () => {
    setHealth('checking…');
    try {
      const res = await fetch(`${config.serverUrl}/health`);
      setHealth(res.ok ? `reachable (${res.status})` : `HTTP ${res.status}`);
    } catch (e) {
      setHealth(`unreachable — ${errorText(e)}`);
    }
  }, [config.serverUrl]);

  const onPick = useCallback(
    async (item: PickerItem) => {
      if (stage) return;
      setResult(null);
      setFailure(null);
      try {
        const uploaded = await uploadAsset(item.id, {
          serverUrl: config.serverUrl,
          deviceId: config.deviceId,
          onStage: setStage,
        });
        setResult(uploaded);
      } catch (e) {
        setFailure(errorText(e));
      } finally {
        setStage(null);
      }
    },
    [config, stage]
  );

  const onChangeServer = useCallback(
    (serverUrl: string) => {
      const next = { ...config, serverUrl };
      setConfig(next);
      saveConfig(next);
    },
    [config]
  );

  return (
    <SafeAreaView style={styles.screen}>
      <StatusBar style="light" />
      <Text style={styles.title}>photobackup</Text>

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
        <Pressable style={styles.button} onPress={checkHealth}>
          <Text style={styles.buttonText}>Check</Text>
        </Pressable>
      </View>
      <Text style={styles.muted}>server: {health}</Text>

      {!granted && (
        <Text style={styles.warning}>
          Photo library access is required. Grant it in Settings to pick a photo.
        </Text>
      )}
      {limitedAccess && (
        <Text style={styles.warning}>
          Limited access is on, so only hand-picked photos are visible. Choose “All Photos” in
          Settings for a real backup.
        </Text>
      )}
      {libraryError && <Text style={styles.warning}>Library error: {libraryError}</Text>}

      {stage ? (
        <View style={styles.status}>
          <ActivityIndicator color="#8ab4f8" />
          <Text style={styles.statusText}>{STAGE_LABEL[stage]}</Text>
        </View>
      ) : (
        <Text style={styles.muted}>Tap a photo to back it up.</Text>
      )}

      {result && (
        <View style={styles.result}>
          <Text style={styles.resultTitle}>
            {result.duplicate ? 'Already archived' : 'Archived'} — {result.filename}
          </Text>
          <Text style={styles.muted}>sha256 {result.sha256.slice(0, 16)}…</Text>
          <Text style={styles.muted}>
            {(result.size / 1024 / 1024).toFixed(2)} MB · hash {result.hashMs} ms · upload{' '}
            {result.uploadMs} ms
          </Text>
        </View>
      )}
      {failure && (
        <View style={styles.result}>
          <Text style={styles.failureTitle}>Upload failed</Text>
          <Text style={styles.muted}>{failure}</Text>
        </View>
      )}

      <FlatList
        data={photos}
        keyExtractor={(item) => item.id}
        numColumns={GRID_COLUMNS}
        contentContainerStyle={styles.grid}
        renderItem={({ item }) => (
          <Pressable style={styles.cell} onPress={() => onPick(item)} disabled={stage !== null}>
            <Thumbnail id={item.id} />
          </Pressable>
        )}
      />
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: '#111', paddingHorizontal: 16 },
  title: { color: '#eee', fontSize: 20, fontWeight: '600', marginTop: 8, marginBottom: 12 },
  row: { flexDirection: 'row', gap: 8, alignItems: 'center' },
  input: {
    flex: 1,
    backgroundColor: '#1c1c1c',
    color: '#eee',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 10,
  },
  button: { backgroundColor: '#2d4a7c', borderRadius: 8, paddingHorizontal: 16, paddingVertical: 11 },
  buttonText: { color: '#eee', fontWeight: '600' },
  muted: { color: '#888', fontSize: 12, marginTop: 6 },
  warning: { color: '#e8b84b', fontSize: 12, marginTop: 8 },
  status: { flexDirection: 'row', alignItems: 'center', gap: 8, marginTop: 10 },
  statusText: { color: '#8ab4f8', fontSize: 13 },
  result: { backgroundColor: '#1c1c1c', borderRadius: 8, padding: 12, marginTop: 10 },
  resultTitle: { color: '#7ed492', fontWeight: '600' },
  failureTitle: { color: '#e2685f', fontWeight: '600' },
  grid: { paddingVertical: 12 },
  cell: { flex: 1 / GRID_COLUMNS, aspectRatio: 1, padding: 2 },
  thumbnail: { flex: 1, borderRadius: 4, backgroundColor: '#222' },
});
