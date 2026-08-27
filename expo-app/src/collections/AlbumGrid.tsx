import {
  closeNotice,
  counted,
  deleteAlbum,
  describeAction,
  notify,
  notifyError,
  UNDO_MS,
  undoDelete,
  unvault,
  vaultAlbum,
  type Album,
  type Bucket,
} from '@photobackup/core';
import { albumsChanged, BUCKET_LABEL, BUCKET_VERB, needsVault } from '@photobackup/core/react';
import { useCallback } from 'react';
import { Pressable, StyleSheet, useWindowDimensions, View } from 'react-native';

import { radius, space } from '../theme';
import { ActionSheet, Text, type Action } from '../ui';
import { Cover } from './Cover';

/** The gap between two album tiles, and what the screen's own padding is. */
const GAP = space.md;
const EDGE = space.lg;

/**
 * The albums, the one holding the most recent photograph first.
 *
 * An album with nothing in it is still drawn. Those come from an import whose
 * directory produced no assets, and one that quietly vanished would be a fact
 * about a failed import that nobody ever finds out.
 *
 * The menu is not here, and the reason is structural: a `Sheet` positions
 * itself against the screen with `absoluteFill`, and a sheet rendered inside a
 * tile inside a scrolling screen would be positioned against the scroll
 * content instead. So the grid reports which album was held and the screen —
 * which is outside its own scroller — draws `AlbumMenu`.
 */
export function AlbumGrid({
  albums,
  onOpen,
  onMenu,
  bucket,
}: {
  albums: Album[];
  onOpen: (album: Album) => void;
  onMenu: (album: Album) => void;
  /**
   * Which half of the archive these albums are in. Undefined is the library's
   * own. Inside a bucket every cover is a decrypted thumbnail, and none of them
   * is written to this phone's disk — see `Cover`.
   */
  bucket?: Bucket;
}) {
  const { width } = useWindowDimensions();
  // Two across. The browser goes to five on a wide screen; a phone has room for
  // two squares and a legible name under each, and three would be neither.
  const side = Math.floor((width - EDGE * 2 - GAP) / 2);

  return (
    <View style={styles.grid}>
      {albums.map((album) => (
        <Pressable
          key={album.id}
          accessibilityRole="button"
          accessibilityLabel={album.title}
          onPress={() => onOpen(album)}
          onLongPress={() => onMenu(album)}
          delayLongPress={380}
          style={({ pressed }) => [{ width: side }, pressed && styles.pressed]}
        >
          <Cover
            id={album.cover_id}
            sealed={bucket !== undefined}
            style={{ width: side, height: side, borderRadius: radius.xl }}
          />
          <Text variant="small" numberOfLines={1} style={styles.name}>
            {album.title}
          </Text>
          <Text variant="caption" tone="faint">
            {album.count.toLocaleString()} {album.count === 1 ? 'item' : 'items'}
          </Text>
        </Pressable>
      ))}
    </View>
  );
}

/**
 * What can be done to one album, and the two ways of deleting it.
 *
 * They are two rows rather than one with a switch because they are two
 * different acts and only one of them is about photographs. Dropping the album
 * leaves every picture in it exactly where it was in the library — it is the
 * grouping an import produced that goes. Dropping the album *and* its contents
 * is a delete of forty photographs that happens to be spelled as an album, and
 * it should have to be aimed at deliberately. Both are armed for that reason.
 *
 * The two above them — Archive and Hide — take the photographs with them into
 * the vault, under one batch, so that the Undo puts the album and its contents
 * back together. Inside a bucket they collapse into the single row that makes
 * sense there: an album already in the Archive can come out of it, and nothing
 * else. There is deliberately no delete in a bucket — taking an album out and
 * then deleting it is two decisions, and one button that decrypted an album in
 * order to throw it away would be spending the password on the one operation
 * that does not need it.
 *
 * @param bucket Which half of the archive this album is in. Undefined is the
 * library's own.
 * @param onChanged Called after a delete lands, and after one is undone, so the
 * screen can re-read the index. There is nothing to patch in place: an album is
 * a row in a list, and the list is one request.
 */
export function AlbumMenu({
  album,
  bucket,
  onClose,
  onChanged,
}: {
  album: Album | null;
  bucket?: Bucket;
  onClose: () => void;
  onChanged?: () => void;
}) {
  // Every write below ends in this, so the sheets that list albums are told at
  // the same moment the screen is. See core's useAlbums.
  const changed = useCallback(() => {
    albumsChanged();
    onChanged?.();
  }, [onChanged]);

  /**
   * Into a bucket, with everything in it.
   *
   * One batch for the album and its photographs, which is what makes the Undo
   * one act rather than forty. A vault that does not exist yet is not an error
   * to report — it is a password to choose — so it is handed to the gate rather
   * than to a notice. See core's `needsVault`.
   */
  const fileAway = useCallback(
    (to: Bucket, id: string, title: string) => {
      onClose();
      vaultAlbum(to, id)
        .then(({ batch, moved }) => {
          changed();
          const notice: string = notify({
            title: `“${title}” ${to === 'archive' ? 'archived' : 'hidden'}`,
            description: `${counted(moved)} encrypted in ${BUCKET_LABEL[to]}, with the album.`,
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
          notifyError(err, `Could not ${BUCKET_VERB[to].toLowerCase()} the album`);
        });
    },
    [changed, onClose],
  );

  /** Back out of a bucket, into the library and into the album it was in. */
  const bringBack = useCallback(
    (from: Bucket, id: string, title: string) => {
      onClose();
      unvault({ bucket: from, album: id })
        .then(({ restored }) => {
          changed();
          notify({
            type: 'success',
            title: `“${title}” restored`,
            description: `${counted(restored)} back in the library, in the album.`,
          });
        })
        .catch((err: unknown) => {
          if (needsVault(err)) return;
          notifyError(err, 'Could not restore the album');
        });
    },
    [changed, onClose],
  );

  const remove = useCallback(
    (id: string, title: string, photos: boolean) => {
      onClose();
      deleteAlbum(id, photos)
        .then(({ batch, deleted }) => {
          changed();
          // Referenced by a handler defined before `notify` returns it, which
          // is safe because that handler cannot run until long after this
          // statement has finished. See core's useTrash.
          const notice: string = notify({
            title: photos ? `Album and ${counted(deleted)} deleted` : `“${title}” deleted`,
            description: photos
              ? 'The photos are in Recently Deleted for 365 days.'
              : 'The photos in it are still in the library.',
            timeout: UNDO_MS,
            action: {
              label: 'Undo',
              onPress: () => {
                closeNotice(notice);
                undoDelete(batch)
                  .then(changed)
                  .catch((err: unknown) => notifyError(err, 'Could not undo'));
              },
            },
          });
        })
        .catch((err: unknown) => notifyError(err, 'Could not delete the album'));
    },
    [changed, onClose],
  );

  // "Archive album", not "Archive Iceland 2025". The album is called by what it
  // is rather than by its name, because a title in a verb phrase reads like a
  // place: "Archive Iceland 2025" is a sentence about Iceland. A person is the
  // other way round, and PeopleRow says so.
  const actions: Action[] = !album
    ? []
    : bucket
      ? [
          {
            key: 'restore',
            label: describeAction(bucket === 'hidden' ? 'Unhide' : 'Unarchive', { kind: 'album' }),
            icon: 'rotate-ccw',
            onPress: () => bringBack(bucket, album.id, album.title),
          },
        ]
      : [
          {
            key: 'archive',
            label: describeAction('Archive', { kind: 'album' }),
            icon: 'archive',
            onPress: () => fileAway('archive', album.id, album.title),
          },
          {
            key: 'hide',
            label: describeAction('Hide', { kind: 'album' }),
            icon: 'eye-off',
            onPress: () => fileAway('hidden', album.id, album.title),
          },
          {
            key: 'delete',
            label: describeAction('Delete', { kind: 'album' }),
            icon: 'folder-minus',
            tone: 'destructive',
            armed: true,
            onPress: () => remove(album.id, album.title, false),
          },
          {
            key: 'delete-all',
            label: 'Delete album and photos',
            icon: 'trash-2',
            tone: 'destructive',
            armed: true,
            disabled: album.count === 0,
            onPress: () => remove(album.id, album.title, true),
          },
        ];

  return (
    <ActionSheet
      open={album !== null}
      onClose={onClose}
      title={album?.title}
      description={album ? `${counted(album.count)} in this album.` : undefined}
      actions={actions}
    />
  );
}

const styles = StyleSheet.create({
  grid: { flexDirection: 'row', flexWrap: 'wrap', gap: GAP },
  name: { marginTop: space.sm, fontWeight: '500' },
  pressed: { opacity: 0.75 },
});
