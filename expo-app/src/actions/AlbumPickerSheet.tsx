import type { Album, Bucket } from '@photobackup/core';
import { useAlbums, useMembership } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { useEffect, useMemo, useState } from 'react';
import { ActivityIndicator, Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { Field, Sheet, Text } from '../ui';

/**
 * "Add to album", wherever something can be filed.
 *
 * One sheet reached from two places — the peek's menu over a single photograph,
 * and the sheet above the selection pill — because those are the two places the
 * rest of the filing actions already live. The browser reaches the same list a
 * submenu and a dropdown; a phone has one shape for "choose one of these", so
 * there is one component rather than two wrappers over shared rows.
 *
 * Search is inside the sheet rather than in front of it. An archive with sixty
 * albums is a scroll; an archive with six is a glance, and putting a second
 * screen between somebody and six rows would be worse for the common case in
 * order to be slightly better for the rare one.
 *
 * @param assetId The one photograph this is about, or null when it is about
 * several. Set, the rows carry ticks for the albums it is already in and
 * tapping a ticked one takes it back out. Null, they do not: a selection of
 * forty has forty answers, and a tick that meant "some of them" is not a thing
 * anybody wants to read off a list.
 */
export function AlbumPickerSheet({
  open,
  bucket,
  assetId,
  onClose,
  onPick,
  onUnpick,
  onCreate,
}: {
  open: boolean;
  /**
   * Which half of the archive the albums come from, and go into. Undefined is
   * the library's own. A hidden photograph can only be filed into a hidden
   * album — the server refuses anything else — so this decides the list as much
   * as it decides the request.
   */
  bucket?: Bucket;
  assetId?: string;
  onClose: () => void;
  onPick: (album: Album) => void;
  /** Takes it back out. Only reachable through a tick. */
  onUnpick?: (album: Album) => void;
  /** Opens the create sheet, with whatever was typed into the search box. */
  onCreate: (name: string) => void;
}) {
  const { albums, error } = useAlbums(bucket, open);
  const { held, mark } = useMembership(assetId ?? null, open);
  const [query, setQuery] = useState('');

  // A sheet opens on whatever was typed into it last otherwise, which is a
  // filtered list somebody has to clear before they can see anything.
  useEffect(() => {
    if (!open) setQuery('');
  }, [open]);

  const trimmed = query.trim();
  const matches = useMemo(() => {
    const needle = trimmed.toLowerCase();
    return (albums ?? []).filter((album) => album.title.toLowerCase().includes(needle));
  }, [albums, trimmed]);

  const create = (
    <Pressable
      accessibilityRole="button"
      onPress={() => onCreate(trimmed)}
      style={({ pressed }) => [styles.row, pressed && styles.pressed]}
    >
      <Feather name="plus" size={17} color={color.primary} />
      <Text variant="body" numberOfLines={1} style={[styles.label, styles.make]}>
        {trimmed ? `Create “${trimmed}”` : 'Create album'}
      </Text>
    </Pressable>
  );

  return (
    <Sheet open={open} onClose={onClose} title="Add to album">
      <Field
        value={query}
        onChangeText={setQuery}
        placeholder="Search albums"
        autoCorrect={false}
        autoCapitalize="none"
        returnKeyType="done"
      />

      {/* With nothing typed, making a new album is the first thing offered,
          above the list. With something typed it is the last, under the matches
          — the thing you meant is almost certainly one of them, and an archive
          where it is not says so by there being no matches at all. */}
      {trimmed ? null : create}

      {error ? (
        <Text variant="small" tone="destructive">
          {error}
        </Text>
      ) : albums === null ? (
        <View style={styles.waiting}>
          <ActivityIndicator size="small" color={color.mutedForeground} />
          <Text variant="small" tone="muted">
            Loading albums
          </Text>
        </View>
      ) : (
        <View style={styles.list}>
          {matches.map((album) => {
            const ticked = held?.has(album.id) === true;
            return (
              <Pressable
                key={album.id}
                accessibilityRole="button"
                accessibilityState={{ selected: ticked }}
                onPress={() => {
                  mark(album.id, !ticked);
                  if (ticked) onUnpick?.(album);
                  else onPick(album);
                }}
                style={({ pressed }) => [styles.row, pressed && styles.pressed]}
              >
                <Feather name="folder" size={17} color={color.mutedForeground} />
                <Text variant="body" numberOfLines={1} style={styles.label}>
                  {album.title}
                </Text>
                {ticked ? <Feather name="check" size={16} color={color.primary} /> : null}
              </Pressable>
            );
          })}
        </View>
      )}

      {trimmed ? create : null}
    </Sheet>
  );
}

const styles = StyleSheet.create({
  list: { gap: space.sm },
  waiting: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    minHeight: 48,
    paddingHorizontal: space.md,
    borderRadius: radius.lg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.secondary,
  },
  label: { flex: 1 },
  make: { color: color.primary },
  pressed: { opacity: 0.72 },
});
