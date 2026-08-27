import { counted, notify, type CreatedAlbum, type Target } from '@photobackup/core';
import { useSelection } from '@photobackup/core/react';
import { useSegments } from 'expo-router';
import { useCallback, useState } from 'react';
import { StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { CreateAlbumSheet, type CreateAlbumRequest } from '../collections/CreateAlbumSheet';
import { space } from '../theme';
import { TAB_BAR_CLEARANCE } from '../ui';
import { AlbumPickerSheet } from './AlbumPickerSheet';
import { FilterPill } from './FilterPill';
import { SelectionPill } from './SelectionPill';
import { stopFiling, useFiling } from './filing';

/**
 * The two things you do *to* a grid, floating above the tab bar, and the two
 * sheets they open.
 *
 * Mounted by the root layout rather than by the tab navigator, for the reason
 * the browser mounts its own in the root layout: the grid that fills a
 * selection is mounted by a screen and the control that reports it is not, so
 * this is the only place the two can meet. It also has to reach the one grid
 * that is *not* in a tab — the search results are a root route — and a control
 * drawn by the tab bar could not.
 *
 * Nothing here draws itself unless a grid has registered, so the collections
 * screen, the backup tab and the pairing gate see none of it.
 */
export function Controls() {
  const insets = useSafeAreaInsets();
  const segments = useSegments();
  const { actions, exit } = useSelection();
  const filing = useFiling();
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);

  // The viewer is a full-screen route above everything, including the tab bar.
  // A control nobody can see should not be there to be tapped through.
  const inTabs = segments[0] === '(tabs)';
  const hidden = segments[0] === 'viewer' || segments[0] === 'pair';

  /**
   * A target the create-album endpoint will understand.
   *
   * A range means "these positions in this timeline", so it travels with the
   * description of the timeline it was counted in. A ranking has no such
   * description — index 2 of "phoenix at the beach" is index 2 of nothing the
   * server can reconstruct — so there the positions are spelled out into ids
   * first. That is what `resolve` is, and the search grid is the only thing
   * that sets it. See core's `useSearchActions`.
   */
  const spellOut = useCallback(
    async (target: Target): Promise<Target> => {
      if (!actions || target.ids) return target;
      if (actions.resolve) return actions.resolve(target);
      return { ...target, filter: actions.filter, view: actions.view };
    },
    [actions],
  );

  const created = useCallback(
    (album: CreatedAlbum) => {
      notify({
        type: 'success',
        title: `“${album.title}” created`,
        description: `${counted(album.added ?? 0)} in it.`,
      });
      exit();
    },
    [exit],
  );

  if (hidden) return null;

  return (
    <>
      <View
        pointerEvents="box-none"
        style={[
          styles.dock,
          {
            // Above the tab bar where there is one, and on the line the bar
            // would have been on where there is not — the search results are a
            // root route and have no tabs under them.
            bottom: insets.bottom + (inTabs ? TAB_BAR_CLEARANCE : space.md),
          },
        ]}
      >
        <FilterPill />
        <SelectionPill />
      </View>

      <AlbumPickerSheet
        open={filing !== null}
        bucket={actions?.bucket}
        assetId={filing?.assetId}
        onClose={stopFiling}
        onPick={(album) => {
          if (!filing || !actions) return;
          stopFiling();
          void actions.file(album, filing.target, filing.noun);
          // Only when this was about a selection. A single photograph filed
          // from the peek must not throw away a selection somebody had already
          // made — and one made *by* ids has nothing to leave.
          if (filing.target.ranges?.length) exit();
        }}
        onUnpick={(album) => {
          if (!filing || !actions) return;
          stopFiling();
          void actions.unfile(album, filing.target, filing.noun);
        }}
        onCreate={(name) => {
          const held = filing;
          stopFiling();
          if (!held) return;
          void spellOut(held.target).then((target) =>
            setCreating({ name, bucket: actions?.bucket, target }),
          );
        }}
      />

      <CreateAlbumSheet
        request={creating}
        onClose={() => setCreating(null)}
        onCreated={created}
      />
    </>
  );
}

const styles = StyleSheet.create({
  dock: {
    position: 'absolute',
    left: space.lg,
    right: space.lg,
    flexDirection: 'row',
    justifyContent: 'flex-end',
    alignItems: 'center',
    gap: space.sm,
    zIndex: 40,
  },
});
