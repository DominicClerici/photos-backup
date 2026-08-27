import { Stack } from 'expo-router';
import { StatusBar } from 'expo-status-bar';
import { StyleSheet } from 'react-native';
import { GestureHandlerRootView } from 'react-native-gesture-handler';
import { SafeAreaProvider } from 'react-native-safe-area-context';

import { ArchiveProvider, useArchive } from '../src/state/archive';
import { BackupProvider } from '../src/state/backup';
import { color } from '../src/theme';
import { installToaster, TAB_BAR_CLEARANCE, Toaster } from '../src/ui';

// Before the first screen mounts, and outside a component, because
// `@photobackup/core`'s `notify()` is called from inside hooks and from module
// scope and has no context to reach. It replaces the `console.warn` that stood
// in for it through Phase 1.
installToaster();

/**
 * The root of the app: the theme, the two providers, and the pairing gate.
 *
 * `src/state/archive.tsx` imports `src/archive.ts`, whose module body is what
 * calls `configure()` on the shared client. So the transport is installed by
 * the time anything below here renders, which is the ordering the core package
 * asks for and the only ordering constraint in the app.
 */
export default function RootLayout() {
  return (
    <GestureHandlerRootView style={styles.root}>
      <SafeAreaProvider>
        <StatusBar style="light" />
        <ArchiveProvider>
          <BackupProvider>
            <Gate />
          </BackupProvider>
        </ArchiveProvider>
      </SafeAreaProvider>
    </GestureHandlerRootView>
  );
}

/**
 * A hard gate: an unpaired phone has no tabs at all.
 *
 * There is nothing behind them for it. Every read route and every write route
 * on photod is behind `requireAuth`, so an unpaired phone's gallery would be
 * one empty state, its collections another and its backup a disabled button —
 * three screens saying the same sentence the pairing form already says.
 *
 * `Stack.Protected` rather than a conditional `<Slot />`: a guard that goes
 * false removes its screens from the navigation state and React Navigation
 * moves focus to what is left, which is how a revoked token puts the pairing
 * screen back up without anything having to call `router.replace`. The two
 * guards are complements so that focus has somewhere to land in both
 * directions.
 *
 * `credentialChecked` is deliberately not consulted here. The keychain read
 * takes a moment, and a third branch for it would mean a blank screen with no
 * routes in it; instead `pair` is what shows while the answer is unknown, and
 * it says so.
 */
function Gate() {
  const { credential } = useArchive();
  const paired = credential !== null;

  return (
    <>
      <Stack
        screenOptions={{
          headerShown: false,
          contentStyle: { backgroundColor: color.background },
        }}
      >
        <Stack.Protected guard={paired}>
          <Stack.Screen name="(tabs)" />
          <Stack.Screen name="settings" options={{ presentation: 'modal' }} />
        </Stack.Protected>

        <Stack.Protected guard={!paired}>
          <Stack.Screen name="pair" />
        </Stack.Protected>
      </Stack>

      {/* Above the router's stack, so a notice outlives the screen that caused
          it. The clearance is for the floating tab bar, which is drawn over the
          content rather than beside it. */}
      <Toaster bottom={paired ? TAB_BAR_CLEARANCE : 0} />
    </>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: color.background },
});
