import { useRouter } from 'expo-router';

import { BackgroundBackupCard } from '../src/components/BackgroundBackupCard';
import { GalleryAccessCard } from '../src/components/GalleryAccessCard';
import { PairingCard } from '../src/components/PairingCard';
import { ServerCard } from '../src/components/ServerCard';
import { SharedAlbums } from '../src/components/SharedAlbums';
import { useBackup } from '../src/state/backup';
import { Screen, Text } from '../src/ui';

/**
 * Where the archive is, what this phone is to it, and the diagnostics.
 *
 * This is the other half of `App.tsx` — everything that was not the backup
 * itself. It is a modal route rather than a tab because it is a screen you open
 * to change something and then close: the gear on the Backup tab is the only
 * way in, which is the right weight for a page you visit twice a year.
 *
 * Forgetting the pairing from here does not need to navigate anywhere. The
 * credential going to null closes the gate in `app/_layout.tsx`, which removes
 * this route along with the tabs and leaves the pairing screen — the same path
 * a revoked token takes.
 */
export default function SettingsRoute() {
  const router = useRouter();
  const { limitedAccess, granted } = useBackup();

  return (
    <Screen title="Settings" action={{ icon: 'x', label: 'Close', onPress: () => router.back() }}>
      <ServerCard />
      <PairingCard />
      <BackgroundBackupCard />
      <GalleryAccessCard />

      {!granted && (
        <Text variant="small" tone="warning">
          Photo library access is required. Grant it in Settings — without it there is nothing to
          survey and nothing to back up.
        </Text>
      )}
      {limitedAccess && (
        <Text variant="small" tone="warning">
          Limited access is on, so only hand-picked photos are visible. Choose “All Photos” in
          Settings for a real backup.
        </Text>
      )}

      <SharedAlbums />
    </Screen>
  );
}
