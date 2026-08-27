import { StyleSheet } from 'react-native';

import { useArchive } from '../../src/state/archive';
import { formatCount } from '../../src/stats/format';
import { Empty, Screen, Text } from '../../src/ui';

/**
 * Empty on purpose. The timeline arrives in Phase 3.
 *
 * The one thing it can honestly say now is how much there is to draw, which it
 * knows because `ArchiveProvider` asks photod for the archive's totals as soon
 * as there is an address and a token. That number is the evidence the transport
 * seam from Phase 1 is working end to end on the phone — the same evidence the
 * gallery-access check in settings gives, without having to be pressed.
 */
export default function GalleryRoute() {
  const { stats } = useArchive();
  const assets = stats?.stats.archive.assets ?? null;

  return (
    <Screen title="Gallery" scrolls={false}>
      <Empty icon="image" title="The timeline lands in Phase 3">
        <Text variant="small" tone="faint" style={styles.centred}>
          {assets === null
            ? 'The archive has not answered yet.'
            : `${formatCount(assets)} items are waiting in the archive.`}
        </Text>
      </Empty>
    </Screen>
  );
}

// React Native does not cascade `textAlign`, so a paragraph inside a centred
// column still sets its own.
const styles = StyleSheet.create({
  centred: { textAlign: 'center' },
});
