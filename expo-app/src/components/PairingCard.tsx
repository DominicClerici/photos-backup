import { useState } from 'react';

import { archiveAddress } from '../archive';
import { useArchive } from '../state/archive';
import { Button, Card, Field, Row, Text } from '../ui';

/**
 * The pairing form, and what it becomes once the phone is paired.
 *
 * One component for both states because they are the same question answered
 * differently: it is either how a phone gets a credential or how it gives one
 * up. On the gate only the first branch is ever reached — an unpaired phone is
 * the only thing that gets there — and in settings only the second.
 *
 * The code and the device name are local state rather than the provider's.
 * Neither survives the screen usefully: a code is good for ten minutes and
 * works once, and a name is only read at the moment Pair is pressed.
 */
export function PairingCard() {
  const {
    credential,
    credentialChecked,
    pairing,
    pairError,
    submitPairing,
    unpair,
  } = useArchive();

  const [code, setCode] = useState('');
  const [deviceName, setDeviceName] = useState('iPhone');

  if (!credentialChecked) {
    return (
      <Card title="Pairing">
        <Text variant="small" tone="muted">
          checking the keychain…
        </Text>
      </Card>
    );
  }

  if (credential) {
    return (
      <Card title="Pairing">
        <Text variant="small" tone="success">
          Paired with {credential.serverName} as {credential.deviceId.slice(0, 8)}
        </Text>
        <Text variant="small" tone="muted">
          The token lives in the keychain and never expires. Unpairing here only forgets it on
          this phone — to stop it working at all, run{' '}
          <Text variant="small" tone="muted" mono>
            photobackup devices --revoke
          </Text>{' '}
          on the server.
        </Text>
        <Button label="Forget this pairing" icon="log-out" onPress={() => void unpair()} />
      </Card>
    );
  }

  const address = archiveAddress();

  return (
    <Card title="Pairing">
      <Text variant="small" tone="muted">
        Run{' '}
        <Text variant="small" tone="muted" mono>
          photobackup pair
        </Text>{' '}
        on the server and type the eight-character code. It is good for ten minutes and works
        once.
      </Text>

      <Row>
        <Field
          value={code}
          onChangeText={setCode}
          code
          autoCapitalize="characters"
          autoCorrect={false}
          autoComplete="off"
          placeholder="ABCD-EFGH"
          editable={!pairing}
          maxLength={9}
        />
        <Button
          label={pairing ? 'Pairing…' : 'Pair'}
          variant="primary"
          // Cleared only on success. A code that was mistyped is worth having
          // back to correct; one that worked is spent.
          onPress={() => {
            void submitPairing(code, deviceName).then((ok) => ok && setCode(''));
          }}
          busy={pairing}
          disabled={!address}
        />
      </Row>

      <Field
        label="This device's name"
        value={deviceName}
        onChangeText={setDeviceName}
        autoCorrect={false}
        editable={!pairing}
      />

      {pairError && (
        <Text variant="small" tone="warning">
          {pairError}
        </Text>
      )}

      {!address && (
        <Text variant="small" tone="muted">
          Find a server first — there is nowhere to send the code yet.
        </Text>
      )}

      <Text variant="small" tone="muted">
        Pairing needs photod&apos;s certificate installed and trusted first, under Settings ›
        General › About › Certificate Trust Settings. Without it every attempt fails as though
        the server were unreachable, because iOS reports a rejected certificate and a dead host
        identically.
      </Text>
    </Card>
  );
}
