import { Stack } from 'expo-router';

import { color } from '../../src/theme';

/**
 * A stack of its own, so that going into a collection inside the bucket and
 * coming back out is this navigator's Back rather than the root's — the same
 * arrangement, and for the same reason, as the Collections tab's.
 */
export default function BucketLayout() {
  return (
    <Stack
      screenOptions={{
        headerShown: false,
        contentStyle: { backgroundColor: color.background },
      }}
    />
  );
}
