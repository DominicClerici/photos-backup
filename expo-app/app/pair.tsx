import { PairingCard } from '../src/components/PairingCard';
import { ServerCard } from '../src/components/ServerCard';
import { Screen, Text } from '../src/ui';

/**
 * The gate. Everything an unpaired phone can do is on this one screen.
 *
 * It is the Server and Pairing sections of the old `App.tsx`, and nothing else:
 * the shared-album tools and the diagnostics need a token to be worth anything
 * and have moved to settings, which is behind the gate. What is left is the two
 * questions in order — where is the archive, and is this phone allowed in.
 */
export default function PairRoute() {
  return (
    <Screen title="photobackup">
      <Text variant="small" tone="muted">
        This phone is not paired with an archive yet. Find the server, then pair with a code from{' '}
        <Text variant="small" tone="muted" mono>
          photobackup pair
        </Text>
        .
      </Text>

      <ServerCard />
      <PairingCard />
    </Screen>
  );
}
