import { useArchive } from '../state/archive';
import { Button, Card, Field, Row, Text } from '../ui';

/**
 * Where the archive is, and how the phone found it.
 *
 * Lifted out of `App.tsx` unchanged in behaviour: one automatic scan at launch
 * (in `ArchiveProvider`), a Find button afterwards, and a typed address as the
 * fallback. It appears twice — on the pairing gate, where it has to work before
 * anything else can, and in settings, where it is how you move the phone onto a
 * different address.
 */
export function ServerCard() {
  const { config, server, resolving, findServer, setServerUrl } = useArchive();

  return (
    <Card title="Server">
      <Text variant="small" tone={server?.url ? 'success' : 'muted'}>
        {resolving ? 'looking for a server…' : (server?.note ?? 'not checked')}
      </Text>

      {server?.emptyScan && (
        <Text variant="small" tone="warning">
          An empty scan means either photod is not running or Local Network access was denied.
          Check Settings › photobackup › Local Network.
        </Text>
      )}

      <Row>
        <Field
          value={config.serverUrl}
          onChangeText={setServerUrl}
          autoCapitalize="none"
          autoCorrect={false}
          keyboardType="url"
          placeholder="https://10.0.0.2:8787"
        />
        <Button label="Find" icon="search" onPress={() => void findServer()} busy={resolving} />
      </Row>
    </Card>
  );
}
