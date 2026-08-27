import {
  fetchVaultCollections,
  type Album,
  type Bucket,
  type CreatedAlbum,
  type Person,
  type VaultCollections,
} from '@photobackup/core';
import {
  albumsChanged,
  askToUnlock,
  BUCKET_LABEL,
  needsVault,
  useVault,
} from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { router } from 'expo-router';
import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { ActivityIndicator, StyleSheet, View } from 'react-native';

import {
  AlbumGrid,
  AlbumMenu,
  CategoryList,
  CreateAlbumSheet,
  PeopleRow,
  PersonMenu,
  type CreateAlbumRequest,
} from '../collections';
import { color, radius, space } from '../theme';
import { Button, ListRow, ROW_ICON_SIZE, RowList, Screen, Text } from '../ui';

/**
 * One bucket's front page: the collections screen, over what is inside the
 * vault.
 *
 * Deliberately the same three sections in the same order, drawn by the same
 * three components. A hidden photograph is still in the albums it was in and
 * still has the people in it that it had — that went into the sealed document
 * with everything else — so there is a real collections page in here, and
 * inventing a second, flatter way of browsing it would be inventing a worse
 * one.
 *
 * What is different is the row above them and the state before them. The row is
 * everything in the bucket as one timeline, because unlike the library this is
 * small enough that "all of it" is a reasonable thing to open. The state is the
 * lock: until the password has been typed this screen knows nothing at all —
 * not the albums, not the count, not a single thumbnail — because the server
 * will not tell it.
 */
export function BucketView({ bucket }: { bucket: Bucket }) {
  const vault = useVault();
  const [data, setData] = useState<VaultCollections | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);
  const [menu, setMenu] = useState<Album | null>(null);
  const [person, setPerson] = useState<Person | null>(null);

  const unlocked = vault.status?.unlocked === true;
  const label = BUCKET_LABEL[bucket];

  useEffect(() => {
    if (!unlocked) {
      setData(null);
      return;
    }
    const abort = new AbortController();
    setError(null);
    fetchVaultCollections(bucket, abort.signal)
      .then(setData)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        if (needsVault(err)) return;
        setError(err instanceof Error ? err.message : 'could not open the vault');
      });
    return () => abort.abort();
  }, [bucket, unlocked, attempt]);

  // Arriving at a locked screen asks straight away rather than making somebody
  // find the button: there is one thing to do here and it is the same thing
  // every time.
  useEffect(() => {
    if (vault.ready && !unlocked) askToUnlock(vault.status);
  }, [vault.ready, vault.status, unlocked]);

  const reload = useCallback(() => retry((n) => n + 1), []);

  // An album made in here is an archived album from the moment it exists: there
  // is nothing in it to move, and the alternative — make it in the library and
  // then hide it — would put its title on the collections screen in between.
  const created = useCallback(
    (album: CreatedAlbum) => {
      albumsChanged();
      router.push(`/${bucket}/albums/${album.id}`);
    },
    [bucket],
  );

  const body = (
    <Screen
      title={label}
      action={
        unlocked
          ? { icon: 'plus', label: 'New album', onPress: () => setCreating({ name: '', bucket }) }
          : undefined
      }
    >
      {/* The one line this screen says that the collections screen does not. A
          page of decrypted thumbnails looks exactly like a page of ordinary
          ones, and it is worth being reminded which of the two is on screen. */}
      <View style={styles.lock}>
        <Feather
          name={unlocked ? 'unlock' : 'lock'}
          size={13}
          color={unlocked ? color.mutedForeground : color.faint}
        />
        <Text variant="caption" tone={unlocked ? 'muted' : 'faint'} style={styles.grow}>
          {unlocked ? `Encrypted · ${label} is open` : `${label} is locked`}
        </Text>
        {vault.ready ? (
          <Button
            label={unlocked ? 'Lock' : 'Unlock'}
            icon={unlocked ? 'lock' : 'unlock'}
            onPress={() => (unlocked ? void vault.lock() : askToUnlock(vault.status))}
          />
        ) : null}
      </View>

      {!vault.ready ? (
        <View style={styles.notice}>
          <ActivityIndicator size="small" color={color.mutedForeground} />
          <Text variant="small" tone="muted">
            Checking the vault
          </Text>
        </View>
      ) : !unlocked ? (
        <Locked bucket={bucket} exists={vault.status?.exists === true} />
      ) : error ? (
        <View style={styles.notice}>
          <Text variant="small" tone="destructive" style={styles.grow}>
            {error}
          </Text>
          <Button label="Try again" onPress={reload} />
        </View>
      ) : !data ? (
        <View style={styles.notice}>
          <ActivityIndicator size="small" color={color.mutedForeground} />
          <Text variant="small" tone="muted">
            Opening {label}
          </Text>
        </View>
      ) : (
        <>
          <Section title="Everything">
            <RowList>
              <ListRow
                label={`All of ${label}`}
                value={data.total.toLocaleString()}
                onPress={() => router.push(`/${bucket}/all`)}
                leading={
                  <View style={styles.badge}>
                    <Feather name="image" size={18} color={color.foreground} />
                  </View>
                }
              />
            </RowList>
          </Section>

          <Section title="People" count={data.people.length} bleed>
            <PeopleRow
              people={data.people}
              onOpen={(who) =>
                router.push(`/${bucket}/people/${encodeURIComponent(who.name)}`)
              }
              onMenu={setPerson}
              bucket={bucket}
            />
          </Section>

          {/* The same components as the collections screen, given the vault's
              routes and told which bucket they are in. A grouping already in a
              bucket has one thing that can be done to it, so their menus offer
              Unarchive or Unhide where the library's offer Archive and Hide. */}
          <Section title="Albums" count={data.albums.length} always>
            <AlbumGrid
              albums={data.albums}
              onOpen={(album) => router.push(`/${bucket}/albums/${album.id}`)}
              onMenu={setMenu}
              bucket={bucket}
            />
          </Section>

          <Section title="Categories" count={data.categories.length}>
            <CategoryList
              categories={data.categories}
              onOpen={(key) => router.push(`/${bucket}/categories/${key}`)}
              sealed
            />
          </Section>

          {data.total === 0 ? (
            <Text variant="small" tone="muted" style={styles.blank}>
              Nothing in {label} yet. Hold a photo, an album or a person and choose{' '}
              {bucket === 'archive' ? 'Archive' : 'Hide'} to put it here.
            </Text>
          ) : null}
        </>
      )}
    </Screen>
  );

  return (
    <>
      {body}

      {/* Outside the screen, and they have to be: a sheet fills the screen with
          `absoluteFill`, so one rendered inside the scroller above would be
          filling the scroll content instead. See `ui/Sheet`. */}
      <AlbumMenu album={menu} bucket={bucket} onClose={() => setMenu(null)} onChanged={reload} />
      <PersonMenu
        person={person}
        bucket={bucket}
        onClose={() => setPerson(null)}
        onChanged={reload}
      />
      <CreateAlbumSheet
        request={creating}
        onClose={() => setCreating(null)}
        onCreated={created}
      />
    </>
  );
}

/**
 * What the screen says with no password in hand.
 *
 * It says nothing about the contents, because it knows nothing about them: the
 * counts, the albums and the thumbnails all come from an endpoint that answers
 * 423 until this is dealt with. That is the whole promise of the feature stated
 * as a screen — there is no partial view, no blurred grid, no "41 items" to
 * read over somebody's shoulder.
 */
function Locked({ bucket, exists }: { bucket: Bucket; exists: boolean }) {
  return (
    <View style={styles.locked}>
      <View style={styles.shield}>
        <Feather name="shield" size={24} color={color.mutedForeground} />
      </View>
      <Text variant="title">{BUCKET_LABEL[bucket]} is locked</Text>
      <Text variant="small" tone="muted" style={styles.centred}>
        {exists
          ? 'The photos and videos in here are encrypted on the drive. Nothing about them — not a thumbnail, not a count — is readable until the vault is unlocked.'
          : 'Nothing has been archived or hidden yet. Choose a password and this becomes an encrypted corner of the archive that only that password opens.'}
      </Text>
      <Button
        label={exists ? 'Unlock' : 'Choose a password'}
        icon="unlock"
        variant="primary"
        onPress={() => askToUnlock(exists ? { exists, unlocked: false } : undefined)}
      />
    </View>
  );
}

/** Collections' own Section, copied rather than shared: see the note there. */
function Section({
  title,
  count,
  always = false,
  bleed = false,
  children,
}: {
  title: string;
  count?: number;
  always?: boolean;
  bleed?: boolean;
  children: ReactNode;
}) {
  if (count === 0 && !always) return null;
  return (
    <View style={[styles.section, bleed && styles.bleed]}>
      <View style={[styles.heading, bleed && styles.headingBleed]}>
        <Text variant="title">{title}</Text>
        {count === undefined ? null : (
          <Text variant="small" tone="faint">
            {count.toLocaleString()}
          </Text>
        )}
      </View>
      {children}
    </View>
  );
}

const styles = StyleSheet.create({
  lock: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingTop: space.xs,
  },
  grow: { flex: 1 },
  section: { gap: space.md, paddingTop: space.md },
  bleed: { marginRight: -space.lg },
  heading: { flexDirection: 'row', alignItems: 'baseline', gap: space.sm },
  headingBleed: { marginRight: space.lg },
  notice: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    paddingVertical: space.xl,
  },
  blank: { paddingVertical: space.xl },
  badge: {
    width: ROW_ICON_SIZE,
    height: ROW_ICON_SIZE,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.lg,
    backgroundColor: color.tile,
    marginVertical: space.xs,
  },
  locked: { alignItems: 'center', gap: space.md, paddingVertical: space.xxl },
  shield: {
    width: 56,
    height: 56,
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.xl,
    backgroundColor: color.tile,
  },
  centred: { textAlign: 'center' },
});
