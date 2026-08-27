import { useCallback, useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { checkGalleryAccess, type CheckResult } from '../gallery/check';
import { useArchive } from '../state/archive';
import { color, space } from '../theme';
import { Button, Card, Text } from '../ui';

/**
 * Proves this phone can read the archive, and that an unpaired one cannot.
 *
 * Scaffolding for the in-app gallery rather than the gallery itself, and it
 * keeps its place in settings for the phase where the gallery exists: when the
 * grid is blank, this is the difference between a revoked token, a server that
 * is not answering, and a bug in the grid.
 *
 * Deliberately free of side effects, unlike the sync engine's handling of the
 * same 401. A diagnostic that threw away the keychain entry when it did not
 * like an answer would be a poor diagnostic; this reports and leaves the Forget
 * button to the person reading it.
 */
export function GalleryAccessCard() {
  const { credential, gallery } = useArchive();
  const [result, setResult] = useState<CheckResult | null>(null);
  const [checking, setChecking] = useState(false);

  const run = useCallback(async () => {
    setChecking(true);
    try {
      setResult(await checkGalleryAccess(gallery));
    } finally {
      setChecking(false);
    }
  }, [gallery]);

  return (
    <Card title="Gallery access">
      <Text variant="small" tone="muted">
        The archive&apos;s read path wants the same device token uploads carry, so browsing it
        from this phone needs no second credential. This checks that end to end.
      </Text>

      <Button
        label={checking ? 'Checking…' : 'Check gallery access'}
        icon="activity"
        onPress={() => void run()}
        busy={checking}
        disabled={!credential}
      />

      {result?.steps.map((step) => (
        <View key={step.label} style={styles.row}>
          <Text variant="body" tone={step.ok ? 'success' : 'destructive'}>
            {step.ok ? '✓' : '✗'}
          </Text>
          <View style={styles.grow}>
            <Text variant="small">{step.label}</Text>
            <Text variant="small" tone="muted">
              {step.detail}
            </Text>
          </View>
        </View>
      ))}

      {result?.unauthorized && (
        <Text variant="small" tone="warning">
          The server refused this phone&apos;s token. It has probably been revoked with{' '}
          <Text variant="small" tone="warning" mono>
            photobackup devices --revoke
          </Text>{' '}
          — forget the pairing above and pair again with a fresh code.
        </Text>
      )}

      {result && !result.ok && !result.unauthorized && (
        <Text variant="small" tone="warning">
          Something in the read path is not as it should be. A failure on the last step in
          particular means photod is answering callers that hold no token.
        </Text>
      )}
    </Card>
  );
}

const styles = StyleSheet.create({
  row: {
    flexDirection: 'row',
    gap: space.sm,
    alignItems: 'flex-start',
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
    paddingTop: space.sm,
  },
  grow: { flex: 1 },
});
