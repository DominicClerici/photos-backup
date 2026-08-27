import { counted, describeAction, type Bucket } from '@photobackup/core';
import { useSelection, type AlbumRef, type SelectionActions } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { useCallback } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { ActionSheet, Button, Text, type Action } from '../ui';
import { askToFile } from './filing';

/**
 * The selection control, to the right of the tab bar.
 *
 * A circle until there is a selection, then a pill with a count in it. It draws
 * nothing at all unless a grid is on screen to select from, which is what keeps
 * it off the collections screen and the backup tab — the same rule the
 * browser's has, enforced the same way, by a grid registering itself with the
 * provider for as long as it is mounted.
 *
 * Pressing it turns selection mode on; pressing it again opens what can be done
 * to what has been picked. Leaving is the Done at the foot of that sheet, and
 * also happens on its own when the grid goes away: a selection is of one
 * timeline, and an index means something else in the next one.
 */
export function SelectionPill() {
  const { active, count, ranges, grid, actions, sheet, setSheet, enter, exit } = useSelection();

  const finish = useCallback(() => {
    setSheet(false);
    exit();
  }, [setSheet, exit]);

  // Whatever the destructive action is here — the library's delete, the trash's
  // purge — pointed at the whole selection.
  //
  // The vault has neither, so there is nothing to point: a selection there can
  // come back out and that is all.
  const destroy = useCallback(() => {
    if (!actions || count === 0 || actions.scope === 'vault') return;
    const target = { ranges };
    void (actions.scope === 'trash' ? actions.purge(target) : actions.remove(target));
    finish();
  }, [actions, count, ranges, finish]);

  const restore = useCallback(() => {
    if (!actions || count === 0) return;
    void actions.restore({ ranges });
    finish();
  }, [actions, count, ranges, finish]);

  const unfile = useCallback(
    (album: AlbumRef) => {
      if (!actions || count === 0) return;
      void actions.unfile(album, { ranges });
      finish();
    },
    [actions, count, ranges, finish],
  );

  // The picker is mounted beside this rather than inside it, because it has to
  // outlive the sheet that asked for one: this one dismisses itself on the way
  // past, and a sheet hosted by a sheet that has closed is a sheet that never
  // opens. See ./filing.
  const file = useCallback(() => {
    if (!actions || count === 0) return;
    setSheet(false);
    askToFile({ target: { ranges } });
  }, [actions, count, ranges, setSheet]);

  if (!grid) return null;

  return (
    <>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={
          active ? (sheet ? 'Hide actions' : 'Show actions') : 'Select photos'
        }
        accessibilityState={{ expanded: active ? sheet : undefined }}
        onPress={() => (active ? setSheet(!sheet) : enter())}
        style={({ pressed }) => [styles.pill, active && styles.lit, pressed && styles.pressed]}
      >
        <Feather
          name={active ? 'chevron-up' : 'check-circle'}
          size={17}
          color={active ? color.foreground : color.mutedForeground}
        />
        {active ? (
          <Text variant="small" style={styles.count}>
            {count.toLocaleString()}
          </Text>
        ) : null}
      </Pressable>

      <SelectionSheet
        open={sheet && active}
        count={count}
        actions={actions}
        onClose={() => setSheet(false)}
        onDestroy={destroy}
        onRestore={restore}
        onFile={file}
        onUnfile={unfile}
        onDone={finish}
      />
    </>
  );
}

/**
 * What can be done to the selection.
 *
 * Which actions those are is the grid's to say rather than this component's:
 * the same gesture means "delete" in the library and "restore or destroy" in
 * Recently Deleted, and this is mounted by the root layout and has no idea
 * which grid is on screen. See core's `SelectionActions`.
 *
 * Archive and Hide are drawn and inert. Both write into the vault, whose gate
 * and password are Phase 6, and a sheet that simply omitted them would make the
 * gallery look like it cannot do a thing it is one phase from doing.
 */
function SelectionSheet({
  open,
  count,
  actions,
  onClose,
  onDestroy,
  onRestore,
  onFile,
  onUnfile,
  onDone,
}: {
  open: boolean;
  count: number;
  actions: SelectionActions | null;
  onClose: () => void;
  onDestroy: () => void;
  onRestore: () => void;
  onFile: () => void;
  onUnfile: (album: AlbumRef) => void;
  onDone: () => void;
}) {
  const trash = actions?.scope === 'trash';
  const vault = actions?.scope === 'vault';
  const album = trash ? undefined : actions?.album;
  const bucket: Bucket | undefined = actions?.bucket;

  // The sheet has no tile under a finger to ask what kind of thing this is, so
  // it says "items" from two upwards and falls back to the generic noun at one.
  // The peek, which does know, says "photo" or "video". See describeAction.
  const about = { kind: 'items', count } as const;

  const list: Action[] = [];

  if (trash || vault) {
    list.push({
      key: 'restore',
      label: vault
        ? describeAction(bucket === 'hidden' ? 'Unhide' : 'Unarchive', about)
        : 'Restore',
      icon: 'rotate-ccw',
      onPress: onRestore,
    });
  }

  if (!trash) {
    list.push({ key: 'file', label: 'Add to album', icon: 'folder-plus', onPress: onFile });
  }

  // Not armed, and not last. Filing something away is undoable from the notice
  // and reversible from a screen one tap away; only the delete below is buying
  // two taps with the thing it cannot buy back. The order says so.
  if (!trash && !vault) {
    list.push(
      {
        key: 'archive',
        label: describeAction('Archive', about),
        icon: 'archive',
        disabled: true,
        note: 'Phase 6',
      },
      {
        key: 'hide',
        label: describeAction('Hide', about),
        icon: 'eye-off',
        disabled: true,
        note: 'Phase 6',
      },
    );
  }

  if (album) {
    list.push({
      key: 'unfile',
      label: `${describeAction('Remove', about)} from album`,
      icon: 'folder-minus',
      armed: true,
      onPress: () => onUnfile(album),
    });
  }

  if (!vault) {
    list.push({
      key: 'delete',
      label: describeAction(trash ? 'Delete forever' : 'Delete', about),
      icon: 'trash-2',
      tone: 'destructive',
      armed: true,
      disabled: !actions,
      onPress: onDestroy,
    });
  }

  return (
    <ActionSheet
      open={open}
      onClose={onClose}
      title={count === 0 ? 'Nothing selected' : counted(count)}
      description={
        count === 0
          ? 'Tap a photo to pick it, or drag sideways across the grid to pick a run of them.'
          : trash
            ? 'Deleting removes the originals from the archive. There is no undo.'
            : vault
              ? 'Decrypted and put back where they were, including any album that still exists.'
              : `Deleting moves ${count === 1 ? 'it' : 'them'} to Recently Deleted, for 365 days.`
      }
      actions={count === 0 ? [] : list}
      footer={
        <View style={styles.footer}>
          <Button label="Done" onPress={onDone} grow />
        </View>
      }
    />
  );
}

const styles = StyleSheet.create({
  pill: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    height: 44,
    paddingHorizontal: space.md,
    borderRadius: radius.pill,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.card,
    shadowColor: '#000',
    shadowOpacity: 0.4,
    shadowRadius: 12,
    shadowOffset: { width: 0, height: 6 },
    elevation: 8,
  },
  lit: { borderColor: color.primary },
  count: { fontVariant: ['tabular-nums'], fontWeight: '600' },
  pressed: { opacity: 0.72 },
  footer: { flexDirection: 'row' },
});
