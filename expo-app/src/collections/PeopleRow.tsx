import {
  BASE_THUMB_SIZE,
  closeNotice,
  counted,
  describeAction,
  notify,
  notifyError,
  UNDO_MS,
  unvault,
  vaultPerson,
  type Bucket,
  type Person,
} from '@photobackup/core';
import { albumsChanged, BUCKET_LABEL, BUCKET_VERB, needsVault } from '@photobackup/core/react';
import { useCallback } from 'react';
import { Pressable, ScrollView, StyleSheet } from 'react-native';

import { space } from '../theme';
import { ActionSheet, Text, type Action } from '../ui';
import { Cover } from './Cover';

/** The circles, and the width a name is allowed to be under one. */
const SIZE = 74;

/**
 * The tagged names, most photographed first.
 *
 * The circles are photographs someone appears in, not faces: nothing in this
 * archive knows where in a frame a person is, so cropping to one would be
 * guessing. It reads as a face row anyway at this size, and it stops being a
 * lie the day the v2 face work gives it something better to draw.
 *
 * A hold opens the menu, which is the same gesture an album tile has and for
 * the same reason — there is no pointer to right-click with. The menu is drawn
 * by the screen rather than here, for the structural reason `AlbumGrid` gives:
 * a sheet inside a horizontal scroller inside a vertical one is a sheet
 * positioned against neither screen edge.
 */
export function PeopleRow({
  people,
  onOpen,
  onMenu,
  bucket,
}: {
  people: Person[];
  onOpen: (person: Person) => void;
  onMenu: (person: Person) => void;
  /** Which half of the archive. Undefined is the library's own. See `Cover`. */
  bucket?: Bucket;
}) {
  return (
    <ScrollView
      horizontal
      showsHorizontalScrollIndicator={false}
      contentContainerStyle={styles.row}
    >
      {people.map((person) => (
        <Pressable
          key={person.name}
          accessibilityRole="button"
          accessibilityLabel={person.name}
          onPress={() => onOpen(person)}
          onLongPress={() => onMenu(person)}
          delayLongPress={380}
          style={({ pressed }) => [styles.person, pressed && styles.pressed]}
        >
          <Cover
            id={person.cover_id}
            size={BASE_THUMB_SIZE}
            sealed={bucket !== undefined}
            round
            style={styles.circle}
          />
          <Text variant="small" tone="muted" numberOfLines={1} style={styles.name}>
            {person.name}
          </Text>
        </Pressable>
      ))}
    </ScrollView>
  );
}

/**
 * What can be done to a person, which is two things and neither of them is a
 * delete.
 *
 * A person is a label an import carried rather than a thing this archive owns,
 * so "delete Brody" has no coherent meaning short of deleting every photograph
 * of them — and that is a selection somebody should have to make in a grid.
 * What is left is the vault: every photograph they are tagged in goes in, and
 * the name goes with it.
 *
 * The label is the name, unlike an album's. "Hide Brody" is what somebody
 * means; "Hide person" would be asking them to remember which circle they had
 * just been holding.
 */
export function PersonMenu({
  person,
  bucket,
  onClose,
  onChanged,
}: {
  person: Person | null;
  /** Which half of the archive they are in. Undefined is the library's own. */
  bucket?: Bucket;
  onClose: () => void;
  onChanged?: () => void;
}) {
  // Hiding everyone a name is tagged on empties whatever albums they were the
  // whole of, so the album lists hear about it at the same moment this screen
  // does — and so does the offline copy of them. See core's `useAlbums`.
  const changed = useCallback(() => {
    albumsChanged();
    onChanged?.();
  }, [onChanged]);

  const fileAway = useCallback(
    (to: Bucket, name: string) => {
      onClose();
      vaultPerson(to, name)
        .then(({ batch, moved }) => {
          changed();
          const notice: string = notify({
            title: `${name} ${to === 'archive' ? 'archived' : 'hidden'}`,
            description: `${counted(moved)} encrypted in ${BUCKET_LABEL[to]}.`,
            timeout: UNDO_MS,
            action: {
              label: 'Undo',
              onPress: () => {
                closeNotice(notice);
                unvault({ batch })
                  .then(changed)
                  .catch((err: unknown) => notifyError(err, 'Could not undo'));
              },
            },
          });
        })
        .catch((err: unknown) => {
          if (needsVault(err)) return;
          notifyError(err, `Could not ${BUCKET_VERB[to].toLowerCase()} ${name}`);
        });
    },
    [changed, onClose],
  );

  const bringBack = useCallback(
    (from: Bucket, name: string) => {
      onClose();
      unvault({ bucket: from, person: name })
        .then(({ restored }) => {
          changed();
          notify({
            type: 'success',
            title: `${name} restored`,
            description: `${counted(restored)} back in the library, where they were.`,
          });
        })
        .catch((err: unknown) => {
          if (needsVault(err)) return;
          notifyError(err, `Could not restore ${name}`);
        });
    },
    [changed, onClose],
  );

  const about = person ? ({ kind: 'person', name: person.name } as const) : null;

  const actions: Action[] =
    !person || !about
      ? []
      : bucket
        ? [
            {
              key: 'restore',
              label: describeAction(bucket === 'hidden' ? 'Unhide' : 'Unarchive', about),
              icon: 'rotate-ccw',
              onPress: () => bringBack(bucket, person.name),
            },
          ]
        : [
            {
              key: 'archive',
              label: describeAction('Archive', about),
              icon: 'archive',
              onPress: () => fileAway('archive', person.name),
            },
            {
              key: 'hide',
              label: describeAction('Hide', about),
              icon: 'eye-off',
              onPress: () => fileAway('hidden', person.name),
            },
          ];

  return (
    <ActionSheet
      open={person !== null}
      onClose={onClose}
      title={person?.name}
      description={
        person
          ? `${counted(person.count)} tagged with this name. Everything here moves them all.`
          : undefined
      }
      actions={actions}
    />
  );
}

const styles = StyleSheet.create({
  row: { gap: space.md, paddingRight: space.lg },
  person: { width: SIZE, alignItems: 'center', gap: space.sm },
  circle: { width: SIZE, height: SIZE, borderRadius: 999 },
  name: { textAlign: 'center' },
  pressed: { opacity: 0.75 },
});
