import {
  fetchCollections,
  type Album,
  type Collections as Data,
  type CreatedAlbum,
  type Person,
} from '@photobackup/core';
import { albumsChanged } from '@photobackup/core/react';
import { router } from 'expo-router';
import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { ActivityIndicator, StyleSheet, View } from 'react-native';

import { COLLECTIONS, recall, remember } from '../gallery/cache';
import { color, space } from '../theme';
import { Button, Screen, Text } from '../ui';
import { AlbumGrid, AlbumMenu } from './AlbumGrid';
import { CategoryList } from './CategoryList';
import { CreateAlbumSheet, type CreateAlbumRequest } from './CreateAlbumSheet';
import { OtherList } from './OtherList';
import { PeopleRow, PersonMenu } from './PeopleRow';

/**
 * Google's imported flag, which is a category on the wire and a destination to
 * read. Drawn under Other rather than among the categories; see OtherList.
 */
const ARCHIVED = 'archived';

/**
 * The ways into the archive that are not "everything, by date".
 *
 * All three sections arrive in one request and none of them page: albums and
 * people are counted in tens here, not thousands, so the whole index is one
 * round trip and the screen has nothing to load as it scrolls.
 */
export function Collections() {
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);
  // Held here rather than in the grid, because a `Sheet` positions itself
  // against the screen and the grid is inside a scroller. See AlbumGrid.
  const [menu, setMenu] = useState<Album | null>(null);
  const [person, setPerson] = useState<Person | null>(null);
  /**
   * Whether what is on screen came out of the cache and the archive has not
   * answered. The same distinction `useTimeline` draws for a day table, for the
   * same reason: with a copy already painted, an unreachable server is a line
   * at the top saying so rather than a screen that is only an error message.
   */
  const [stale, setStale] = useState(false);

  useEffect(() => {
    const abort = new AbortController();
    setError(null);
    let answered = false;

    // Geometry before connectivity, one screen up from the grid. Every album
    // behind this list has a week of cached day table, and a Collections tab
    // that was a spinner and then a failure would be the one thing standing
    // between somebody and all of it. See src/gallery/cache.
    void recall<Data>(COLLECTIONS).then((held) => {
      if (!held || answered || abort.signal.aborted) return;
      setData(held);
      setStale(true);
    });

    fetchCollections(abort.signal)
      .then((fresh) => {
        if (abort.signal.aborted) return;
        answered = true;
        setData(fresh);
        setStale(false);
        // Everything except how much is in each bucket. That field is present
        // only while the vault is unlocked and is a fact about what somebody
        // hid — "Hidden 41" read off a screen is the number the server withholds
        // for exactly that reason, and writing it down here would be this app
        // keeping it after the server had stopped saying it. The two rows fall
        // back to "Locked", which is what they say before any password anyway.
        // See src/gallery/cache.
        const { vault: _sealed, ...rest } = fresh;
        remember(COLLECTIONS, rest);
      })
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : 'could not load collections');
      });
    return () => abort.abort();
  }, [attempt]);

  // Made from the heading rather than from a selection, so there is nothing to
  // put in it and nowhere to stay: the useful next thing is the album itself,
  // empty, ready to be filled from any grid.
  const created = useCallback((album: CreatedAlbum) => {
    albumsChanged();
    router.push(`/collections/albums/${album.id}`);
  }, []);

  const categories = data?.categories.filter((c) => c.key !== ARCHIVED) ?? [];
  const archivedCount = data?.categories.find((c) => c.key === ARCHIVED)?.count;

  const empty =
    data !== null &&
    data.albums.length === 0 &&
    data.people.length === 0 &&
    categories.length === 0;

  const body = (
    <Screen
      title="Collections"
      action={{ icon: 'plus', label: 'New album', onPress: () => setCreating({ name: '' }) }}
    >
      {error ? (
        <View style={styles.notice}>
          <Text variant="small" tone={stale ? 'muted' : 'destructive'} style={styles.grow}>
            {stale ? `${error}. Showing what was here last time.` : error}
          </Text>
          <Button label="Try again" onPress={() => retry((n) => n + 1)} />
        </View>
      ) : null}

      {!data && !error ? (
        <View style={styles.notice}>
          <ActivityIndicator size="small" color={color.mutedForeground} />
          <Text variant="small" tone="muted">
            Loading collections
          </Text>
        </View>
      ) : null}

      {/* People and categories come from an import or from the phone's own
          description of a shot, and an archive built only from plain backups
          has neither. Albums are the one thing on this screen that can be made
          from here, which is what the sentence has to say — the + is a few
          points above it. */}
      {empty ? (
        <Text variant="small" tone="muted" style={styles.blank}>
          Nothing to group by yet. Make an album, or import an export that
          carries people and categories.
        </Text>
      ) : null}

      {data ? (
        <>
          <Section title="People" count={data.people.length} bleed>
            <PeopleRow
              people={data.people}
              onOpen={(who) =>
                router.push(`/collections/people/${encodeURIComponent(who.name)}`)
              }
              onMenu={setPerson}
            />
          </Section>

          {/* Drawn even when it is empty, because an archive with no albums is
              exactly the archive somebody needs the + for. */}
          <Section title="Albums" count={data.albums.length} always>
            <AlbumGrid
              albums={data.albums}
              onOpen={(album) => router.push(`/collections/albums/${album.id}`)}
              onMenu={setMenu}
            />
          </Section>

          <Section title="Categories" count={categories.length}>
            <CategoryList
              categories={categories}
              onOpen={(key) => router.push(`/collections/categories/${key}`)}
            />
          </Section>

          {/* No count beside the heading: the rows are fixed, so the number
              would only ever say how many of them there are. */}
          <Section title="Other">
            <OtherList
              counts={{
                archived: archivedCount,
                trash: data.trash,
                archive: data.vault?.archive,
                hidden: data.vault?.hidden,
              }}
              onOpen={(key) =>
                router.push(
                  key === ARCHIVED ? `/collections/categories/${ARCHIVED}` : `/${key}`,
                )
              }
            />
          </Section>
        </>
      ) : null}
    </Screen>
  );

  return (
    <>
      {body}

      {/* Outside the screen, and they have to be: both are sheets, and a sheet
          fills the screen with `absoluteFill` — inside the scroller above it
          would be filling the scroll content instead. */}
      <AlbumMenu
        album={menu}
        onClose={() => setMenu(null)}
        onChanged={() => retry((n) => n + 1)}
      />
      <PersonMenu
        person={person}
        onClose={() => setPerson(null)}
        onChanged={() => retry((n) => n + 1)}
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
 * A heading and its contents. A section given a count of zero has nothing to
 * draw and draws nothing; one marked `always` is there regardless.
 */
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
  /**
   * Lets the contents run past the screen's right edge. The people row scrolls
   * sideways, and a horizontal scroller that stops short of the edge reads as a
   * list that has ended rather than one that continues.
   */
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
  grow: { flex: 1 },
  blank: { paddingVertical: space.xl },
});
