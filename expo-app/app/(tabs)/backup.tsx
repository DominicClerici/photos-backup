import { useRouter } from 'expo-router';
import { ActivityIndicator, StyleSheet, View } from 'react-native';

import { ArchiveSummary } from '../../src/components/ArchiveSummary';
import { useArchive } from '../../src/state/archive';
import { useBackup } from '../../src/state/backup';
import { color, space } from '../../src/theme';
import { Button, Card, Count, Counts, Field, Row, Screen, Text } from '../../src/ui';

/**
 * The backup engine, given a face.
 *
 * Everything here was in `App.tsx` and does what it did. What changed is where
 * the state lives: the engine, the queue and the progress are in
 * `BackupProvider` rather than in this component, because a run has to survive
 * this screen being unmounted — switching to the gallery mid-upload must not
 * tear down a `SyncEngine` with a queue row still in flight.
 *
 * The gear is the only way to settings, which is the right weight for it: the
 * server, the pairing and the shared-album tools are the same job as the queue,
 * and nothing on the two gallery tabs wants them.
 */
export default function BackupRoute() {
  const router = useRouter();
  const { config, setMaxItems } = useArchive();
  const {
    granted,
    limitedAccess,
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
  } = useBackup();

  return (
    <Screen
      title="Backup"
      action={{ icon: 'settings', label: 'Settings', onPress: () => router.push('/settings') }}
    >
      {!granted && (
        <Text variant="small" tone="warning">
          Photo library access is required. Grant it in Settings to back anything up.
        </Text>
      )}
      {limitedAccess && (
        <Text variant="small" tone="warning">
          Limited access is on, so only hand-picked photos are visible. Choose “All Photos” in
          Settings for a real backup.
        </Text>
      )}
      {storeError && (
        <Text variant="small" tone="warning">
          Queue unavailable: {storeError}
        </Text>
      )}
      {runError && (
        <Text variant="small" tone="warning">
          Run stopped: {runError}
        </Text>
      )}

      <Card title="Archive">
        <ArchiveSummary />
      </Card>

      <Card title="This run">
        <Row>
          <Button
            label={running ? 'Running…' : 'Start backup'}
            variant="primary"
            icon="upload-cloud"
            grow
            onPress={() => void start()}
            busy={running}
            disabled={!canStart}
          />
          <Button label="Pause" icon="pause" onPress={pause} disabled={!running} />
        </Row>

        {heldBecause && !running && (
          <>
            <Text variant="small" tone="warning">
              Holding off — {heldBecause}. A full backup warms the phone up and eats battery, so
              it waits for a charger and Wi-Fi.
            </Text>
            <Button label="Back up anyway" onPress={() => void start(true)} />
          </>
        )}

        <View style={styles.status}>
          {running && <ActivityIndicator size="small" color={color.primary} />}
          <Text variant="small" tone="primary" style={styles.grow}>
            {progress.activity}
          </Text>
        </View>

        <Counts>
          <Count
            label="queued"
            value={String(progress.counts.pending + progress.counts.unknown + progress.counts.hashed)}
          />
          <Count label="to send" value={String(progress.counts.want)} />
          <Count
            label="failed"
            value={String(progress.counts.failed)}
            tone={progress.counts.failed > 0 ? 'destructive' : 'default'}
          />
        </Counts>
      </Card>

      <Card title="Scope">
        <Field
          label="Newest items to consider"
          value={String(config.maxItems)}
          onChangeText={setMaxItems}
          keyboardType="number-pad"
          align="right"
          editable={!running}
        />
        <Text variant="small" tone="muted">
          0 backs up the whole library.
        </Text>

        <Button
          label="Re-check the archive"
          icon="refresh-cw"
          onPress={() => void recheckArchive()}
          disabled={running}
        />
        <Text variant="small" tone="muted">
          Forgets what this phone believes it has already backed up, without touching a photo. The
          next run asks the archive about every item again and re-sends only what is genuinely
          missing. Use this if the archive was rebuilt, or if a photo you can see on this phone
          never appears in the gallery.
        </Text>
      </Card>

      {failed.length > 0 && (
        <Card title={`Failed (${failed.length})`}>
          <Button
            label="Retry failed items"
            icon="rotate-ccw"
            onPress={() => void retryFailed()}
            disabled={running}
          />
          {failed.map((item) => (
            <View key={item.localId} style={styles.failedRow}>
              <Text variant="small">{item.filename}</Text>
              <Text variant="caption" tone="muted">
                {item.lastError ?? 'no reason recorded'}
              </Text>
            </View>
          ))}
        </Card>
      )}

      {logLines.length > 0 && (
        <Card title="Log">
          {logLines.map((line, index) => (
            <Text key={`${index}-${line}`} variant="caption" tone="muted" mono>
              {line}
            </Text>
          ))}
        </Card>
      )}
    </Screen>
  );
}

const styles = StyleSheet.create({
  status: { flexDirection: 'row', alignItems: 'center', gap: space.sm, minHeight: 22 },
  grow: { flex: 1 },
  failedRow: {
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
    paddingTop: space.xs + 2,
  },
});
