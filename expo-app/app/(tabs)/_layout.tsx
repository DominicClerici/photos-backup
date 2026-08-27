import { Tabs } from 'expo-router';

import { color } from '../../src/theme';
import { TabBar } from '../../src/ui';

/**
 * Gallery · Collections · Backup.
 *
 * The same three places the browser has, minus Status and Upload — see
 * `WEB_TO_MOBILE.md` § 6 for why neither is coming. The bar itself is drawn by
 * `src/ui/TabBar.tsx` and floats over the content rather than sitting under it,
 * so a screen that fills the width (the timeline, in Phase 3) runs to the
 * bottom edge with the bar on top of it. Every scrolling screen leaves
 * `TAB_BAR_CLEARANCE` at its foot for that reason.
 *
 * Search is not a tab here either, and for the browser's reason: a search is a
 * question rather than a destination. Its floating button and the `/search`
 * route it opens land with Phase 5, which is where there is finally something
 * to search.
 */
export default function TabsLayout() {
  return (
    <Tabs
      tabBar={(props) => <TabBar {...props} />}
      screenOptions={{
        headerShown: false,
        sceneStyle: { backgroundColor: color.background },
      }}
    >
      <Tabs.Screen name="index" options={{ title: 'Gallery' }} />
      <Tabs.Screen name="collections" options={{ title: 'Collections' }} />
      <Tabs.Screen name="backup" options={{ title: 'Backup' }} />
    </Tabs>
  );
}
