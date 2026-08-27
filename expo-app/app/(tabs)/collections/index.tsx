import { Empty, Screen } from '../../../src/ui';

/**
 * Empty on purpose. Albums, people and categories arrive in Phase 5.
 *
 * An empty screen that says nothing is indistinguishable from one that failed
 * to load, which is the whole reason this file has any content at all.
 */
export default function CollectionsRoute() {
  return (
    <Screen title="Collections" scrolls={false}>
      <Empty icon="folder" title="Nothing here yet" />
    </Screen>
  );
}
