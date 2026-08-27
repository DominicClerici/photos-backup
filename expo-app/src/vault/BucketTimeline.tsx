import {
  fetchVaultCollections,
  type Album,
  type Bucket,
  type CollectionFilter,
  type TimelineFilter,
} from '@photobackup/core';
import {
  askToUnlock,
  BUCKET_LABEL,
  useTrashActions,
  useVault,
  useView,
} from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { router } from 'expo-router';
import { useEffect, useMemo, useState } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { categoryLabel } from '../collections';
import { useCachedTimeline } from '../gallery/cache';
import { Grid } from '../grid';
import { useBrowsing } from '../state/browsing';
import { color, space } from '../theme';
import { Button, Text } from '../ui';

/**
 * A timeline inside a bucket: all of it, or one of its collections.
 *
 * The same grid, the same viewer, the same zoom and the same paging as the
 * library, over a timeline the server computes in memory from decrypted rows —
 * which the client cannot tell and does not need to. That is the point of the
 * vault's endpoints answering in the gallery's own shapes: there is one gallery
 * in this app, and encrypting half the archive did not earn a second one.
 *
 * What is different is the one thing that is different everywhere in the vault:
 * with no password there is nothing here at all. Not a blurred grid, not a
 * count, not a placeholder tile — the day table this screen's geometry is built
 * from is itself behind the lock.
 *
 * And nothing here is ever written to the offline cache. `useCachedTimeline`
 * hands a vault filter no store, which is what keeps the sealed documents'
 * dates and filenames off this phone's disk; the tiles below do the same for
 * their bytes. See `src/gallery/cache.ts`.
 */
export function BucketTimeline({
  bucket,
  within,
}: {
  bucket: Bucket;
  /** The collection inside the bucket, or undefined for all of it. */
  within?: CollectionFilter;
}) {
  const vault = useVault();
  const unlocked = vault.status?.unlocked === true;

  // Memoised on its parts rather than built inline: the actions and the
  // timeline both close over this object's identity, and a fresh one every
  // render would rebuild both every render.
  const filter = useMemo<TimelineFilter>(
    () => ({ kind: 'vault', bucket, within }),
    [bucket, within?.kind, within?.value],
  );

  useEffect(() => {
    if (vault.ready && !unlocked) askToUnlock(vault.status);
  }, [vault.ready, vault.status, unlocked]);

  const heading = useBucketHeading(bucket, within, unlocked);

  return (
    <View style={styles.root}>
      <Header bucket={bucket} title={heading.title} description={heading.description} />
      {unlocked ? (
        <Unlocked filter={filter} album={heading.album} />
      ) : (
        <View style={styles.shut}>
          <Feather name="lock" size={24} color={color.faint} />
          <Text variant="small" tone="muted">
            {BUCKET_LABEL[bucket]} is locked.
          </Text>
          {vault.ready ? (
            <Button
              label="Unlock"
              icon="unlock"
              variant="primary"
              onPress={() => askToUnlock(vault.status)}
            />
          ) : null}
        </View>
      )}
    </View>
  );
}

/**
 * The screen once the key is in memory.
 *
 * A separate component because `useTimeline` starts fetching the moment it is
 * mounted, and mounting it against a locked vault would be a day-table request
 * that is going to answer 423 — one wasted round trip per render, and an error
 * notice on a screen whose actual problem is a prompt somebody has not typed
 * into yet.
 */
function Unlocked({ filter, album }: { filter: TimelineFilter; album?: string }) {
  const { view } = useView();
  const { timeline, reload } = useCachedTimeline(filter, view);
  const actions = useTrashActions(filter, reload, album, view);
  useBrowsing(timeline, true);

  return (
    <Grid
      timeline={timeline}
      filter={filter}
      actions={actions}
      immersive={false}
      empty="Nothing in here."
    />
  );
}

function Header({
  bucket,
  title,
  description,
}: {
  bucket: Bucket;
  title: string;
  description?: string;
}) {
  const insets = useSafeAreaInsets();

  return (
    <>
      <View style={[styles.header, { paddingTop: insets.top + space.sm }]}>
        <Pressable
          accessibilityRole="button"
          accessibilityLabel={`Back to ${BUCKET_LABEL[bucket]}`}
          onPress={() => router.back()}
          hitSlop={10}
          style={({ pressed }) => pressed && styles.pressed}
        >
          <Feather name="chevron-left" size={22} color={color.mutedForeground} />
        </Pressable>

        <Text variant="title" numberOfLines={1} style={styles.title}>
          {title}
        </Text>
        {/* Which of the two things on screen this is. A decrypted thumbnail
            looks exactly like an ordinary one. */}
        <Feather name="lock" size={13} color={color.faint} />
      </View>

      {description ? (
        <Text variant="small" tone="faint" numberOfLines={2} style={styles.description}>
          {description}
        </Text>
      ) : null}
    </>
  );
}

/**
 * What to call this screen, and what else its heading has to say.
 *
 * Two of the three kinds carry their name in the route. An album carries a
 * uuid, and unlike the library's it cannot be looked up by id either — the
 * album endpoint only knows about albums in the library, and a hidden one is
 * deliberately not there. So the title comes from the bucket's own collections
 * page, which is one request and the same one that drew the tile that was
 * tapped to get here.
 *
 * A heading that says "Album" is a worse screen than one that says "Iceland
 * 2025", and a better one than no photographs at all.
 */
function useBucketHeading(
  bucket: Bucket,
  within: CollectionFilter | undefined,
  unlocked: boolean,
): { title: string; description?: string; album?: string } {
  const [album, setAlbum] = useState<Album | null>(null);

  const wanted = within?.kind === 'albums' ? within.value : '';

  useEffect(() => {
    setAlbum(null);
    if (!wanted || !unlocked) return;
    const abort = new AbortController();
    fetchVaultCollections(bucket, abort.signal)
      .then((data) => setAlbum(data.albums.find((a) => a.id === wanted) ?? null))
      .catch(() => {});
    return () => abort.abort();
  }, [bucket, wanted, unlocked]);

  if (!within) return { title: `All of ${BUCKET_LABEL[bucket]}` };
  switch (within.kind) {
    case 'albums':
      return {
        title: album?.title ?? 'Album',
        description: album?.description,
        album: album?.title,
      };
    case 'people':
      return { title: within.value };
    case 'categories':
      return { title: categoryLabel(within.value) };
  }
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: color.background },
  header: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingBottom: space.sm,
  },
  title: { flex: 1 },
  description: { paddingHorizontal: space.lg, paddingBottom: space.sm },
  pressed: { opacity: 0.6 },
  shut: { flex: 1, alignItems: 'center', justifyContent: 'center', gap: space.md },
});
