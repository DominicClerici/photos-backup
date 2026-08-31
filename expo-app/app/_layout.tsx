import { Stack } from 'expo-router';
import { StatusBar } from 'expo-status-bar';
import { StyleSheet } from 'react-native';
import { GestureHandlerRootView } from 'react-native-gesture-handler';
import { SafeAreaProvider } from 'react-native-safe-area-context';

import { SelectionProvider, ViewProvider } from '@photobackup/core/react';

import { Controls } from '../src/actions';
// Imported for its module body, which is where the background task is defined.
// iOS launching the app to service a task loads this bundle and then looks for a
// handler under that name, so the definition has to have happened by the time
// any of this file's exports are called. See src/background/task.ts.
import '../src/background/task';
import { installCacheInvalidation } from '../src/gallery/cache';
import { installSearchRecents, useSearchWarmth } from '../src/search';
import { ArchiveProvider, useArchive } from '../src/state/archive';
import { BackupProvider } from '../src/state/backup';
import { color } from '../src/theme';
import { installToaster, TAB_BAR_CLEARANCE, Toaster } from '../src/ui';
import { VaultGate } from '../src/vault';

// Before the first screen mounts, and outside a component, because
// `@photobackup/core`'s `notify()` is called from inside hooks and from module
// scope and has no context to reach. It replaces the `console.warn` that stood
// in for it through Phase 1.
installToaster();

// Likewise outside a component: core reads the recent-search list synchronously,
// on the first frame of the blank search screen, and a list that arrived a tick
// later would appear under somebody's thumb. See src/search/recents.
installSearchRecents();

// Likewise once, outside a component, and for the third time in this file the
// same reason: core broadcasts that the albums changed from inside a hook, and
// the thing that has to hear it is a SQLite database rather than a screen. What
// it does about it is drop the offline copy of a timeline that has moved on.
// See src/gallery/cache.
installCacheInvalidation();

/**
 * The root of the app: the theme, the providers, the floating controls and the
 * pairing gate.
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
            {/* The selection lives above the router for the reason the browser
                puts it above its own pages: the grid that fills one is mounted
                by a screen and the control that reports it is not, so this is
                the only place the two can meet. The sort and the filters are
                the same arrangement for the same reason, and one layer further
                in — a view is of a selection's timeline, and changing it drops
                the selection made under the old one. */}
            <SelectionProvider>
              <ViewProvider>
                <Gate />
              </ViewProvider>
            </SelectionProvider>
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

  // The app being open is the fact photo-ml cannot observe and needs: it is
  // what decides whether the models a search runs on are in VRAM or whether the
  // card is free for the overnight captioning pass. Asked here rather than on
  // the search screen, because the whole point is that the checkpoints finish
  // loading before somebody types — and asked only when paired, because an
  // unpaired phone has no credential to ask with and nothing behind the ask.
  useSearchWarmth(paired);

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
          {/* A root route rather than a tab: a search is a question rather than
              a destination, which is the browser's reading too. Slid in from
              the side, because backing out of it is going back to where the
              question was asked. */}
          <Stack.Screen name="search" />
          {/* What has left the library, and what is sealed inside it. Root
              routes rather than screens in the Collections tab, which is what
              WEB_TO_MOBILE § 3.3 asks for and what the shape wants: each of
              them is a full-width grid, and a grid with a floating tab bar over
              it is the library's screen wearing somebody else's chrome. Their
              own stacks, so Back inside a bucket goes to the bucket. */}
          <Stack.Screen name="trash" />
          <Stack.Screen name="archive" />
          <Stack.Screen name="hidden" />
          {/* Transparent, and faded rather than slid up: the viewer draws its
              own backdrop, and a drag towards dismissing it thins that backdrop
              until the grid the photograph came out of shows through. A modal
              with a background of its own would be an opaque card sliding down
              over the same grid, which is a different gesture wearing the same
              clothes. Full screen either way — it is a root route, so it is
              above the tab navigator and covers the floating bar. */}
          <Stack.Screen
            name="viewer/[index]"
            options={{
              presentation: 'transparentModal',
              animation: 'fade',
              animationDuration: 180,
              contentStyle: { backgroundColor: 'transparent' },
            }}
          />
        </Stack.Protected>

        <Stack.Protected guard={!paired}>
          <Stack.Screen name="pair" />
        </Stack.Protected>
      </Stack>

      {/* Above the router's stack, and drawing nothing at all until a grid has
          registered — so the collections screen, the backup tab and the pairing
          form never see it. */}
      {paired ? <Controls /> : null}

      {/* Above the router too, and it has to be: archiving a photograph on an
          archive that has never had a vault asks for a password from the middle
          of the library's grid, and a locked bucket asks from a screen that has
          drawn nothing. One prompt, opened from anywhere. See src/vault/Gate. */}
      {paired ? <VaultGate /> : null}

      {/* Above both, so a notice outlives the screen that caused it. The
          clearance is for the floating tab bar, which is drawn over the content
          rather than beside it. */}
      <Toaster bottom={paired ? TAB_BAR_CLEARANCE : 0} />
    </>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: color.background },
});
