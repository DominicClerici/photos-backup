import { Stack } from 'expo-router';

import { color } from '../../../src/theme';

/**
 * A stack inside the Collections tab, holding one screen so far.
 *
 * It exists now rather than in Phase 5 because of what Phase 5 adds:
 * `collections/[kind]/[value]` — an album, a person, a category — is a place
 * you go *into* and come back out of, and the Back gesture that returns you is
 * this navigator's. Adding it later would mean moving the route, and a route
 * that moves is a deep link that breaks.
 */
export default function CollectionsLayout() {
  return (
    <Stack
      screenOptions={{
        headerShown: false,
        contentStyle: { backgroundColor: color.background },
      }}
    />
  );
}
