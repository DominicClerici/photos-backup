"use client";

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Loader2, Plus } from "lucide-react";
import { useRouter } from "next/navigation";

import { fetchCollections, type Collections as Data, type CreatedAlbum } from "@/lib/api";
import { albumsChanged } from "@/hooks/useAlbums";
import { useVault } from "@/hooks/useVault";
import { Button } from "@/components/ui/button";
import { AlbumGrid } from "./AlbumGrid";
import { CreateAlbumDialog, type CreateAlbumRequest } from "./CreateAlbumDialog";
import { CategoryList } from "./CategoryList";
import { OtherList } from "./OtherList";
import { PeopleRow } from "./PeopleRow";

/**
 * The ways into the archive that are not "everything, by date".
 *
 * All three sections arrive in one request and none of them page: albums and
 * people are counted in tens here, not thousands, so the whole index is one
 * round trip and the page has nothing to load as it scrolls.
 */
const ARCHIVED = "archived";

export function Collections() {
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);
  const vault = useVault();
  const router = useRouter();

  // Made from the heading rather than from a selection, so there is nothing to
  // put in it and nowhere to stay: the useful next thing is the album itself,
  // empty, ready to be filled from any grid.
  const created = useCallback(
    (album: CreatedAlbum) => {
      albumsChanged();
      router.push(`/collections/albums/${album.id}`);
    },
    [router],
  );

  useEffect(() => {
    const abort = new AbortController();
    setError(null);
    fetchCollections(abort.signal)
      .then(setData)
      .catch((err: unknown) => {
        if (abort.signal.aborted) return;
        setError(err instanceof Error ? err.message : "could not load collections");
      });
    return () => abort.abort();
  }, [attempt]);

  // Google's imported "archived" flag is still a category on the wire — it is a
  // predicate over the asset row like every other one, and the timeline it
  // links to is the same filtered timeline. It is only drawn elsewhere, under
  // Other, beside the destinations it reads like and is not. It has nothing to
  // do with this archive's own Archive; see OtherList.
  const categories = data?.categories.filter((c) => c.key !== ARCHIVED) ?? [];
  const archivedCount = data?.categories.find((c) => c.key === ARCHIVED)?.count;

  const empty =
    data !== null &&
    data.albums.length === 0 &&
    data.people.length === 0 &&
    categories.length === 0;

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-4">
        <h1 className="text-[15px] font-semibold tracking-[0.01em]">Collections</h1>
      </header>

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        {error ? (
          <Notice>
            <span className="text-destructive">{error}</span>
            <Button variant="outline" size="sm" onClick={() => retry((n) => n + 1)}>
              Try again
            </Button>
          </Notice>
        ) : null}

        {!data && !error ? (
          <Notice>
            <Loader2 className="size-4 animate-spin" aria-hidden="true" />
            Loading collections
          </Notice>
        ) : null}

        {/* People and categories come from an import or from the phone's own
            description of a shot, and an archive built only from plain uploads
            has neither. Albums are the one thing on this page that can be made
            from here, which is what the sentence has to say — the + is a few
            pixels above it. */}
        {empty ? (
          <Notice>
            Nothing to group by yet. Make an album, or import an export that
            carries people and categories.
          </Notice>
        ) : null}

        {data ? (
          <div className="mx-auto max-w-7xl">
            <Section title="People" count={data.people.length}>
              <PeopleRow people={data.people} onChanged={() => retry((n) => n + 1)} />
            </Section>

            {/* The one section with a control beside its heading, and the one
                that is drawn even when it is empty — an archive with no albums
                is exactly the archive somebody needs the + for. */}
            <Section
              title="Albums"
              count={data.albums.length}
              action={<NewAlbumButton onClick={() => setCreating({ name: "" })} />}
            >
              <AlbumGrid albums={data.albums} onChanged={() => retry((n) => n + 1)} />
            </Section>

            <Section title="Categories" count={categories.length}>
              <CategoryList categories={categories} />
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
                unlocked={vault.status?.unlocked}
              />
            </Section>
          </div>
        ) : null}
      </div>

      <CreateAlbumDialog
        request={creating}
        onClose={() => setCreating(null)}
        onCreated={created}
      />
    </div>
  );
}

/** The + beside the Albums heading. The only way into the create dialog that is
 * not about a selection. */
export function NewAlbumButton({ onClick }: { onClick: () => void }) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-sm"
      aria-label="New album"
      title="New album"
      className="-my-1 ml-auto text-muted-foreground hover:text-foreground"
      onClick={onClick}
    >
      <Plus aria-hidden="true" />
    </Button>
  );
}

/**
 * A heading and its contents. A section given a count of zero has nothing to
 * draw and draws nothing; one given no count at all is always there.
 */
function Section({
  title,
  count,
  action,
  children,
}: {
  title: string;
  count?: number;
  /** A control on the heading's own row. A section that has one is always drawn:
   * an empty Albums section still has a + in it, and that is the whole point. */
  action?: ReactNode;
  children: ReactNode;
}) {
  if (count === 0 && !action) return null;
  return (
    <section className="pt-7">
      <div className="mb-3 flex items-center gap-2">
        <h2 className="flex items-baseline gap-2 text-sm font-semibold tracking-[0.01em]">
          {title}
          {count === undefined ? null : (
            <span className="text-[13px] font-normal text-faint">{count.toLocaleString()}</span>
          )}
        </h2>
        {action}
      </div>
      {children}
    </section>
  );
}

function Notice({ children }: { children: ReactNode }) {
  return (
    <p className="flex items-center justify-center gap-3 px-3 py-7 text-sm text-muted-foreground">
      {children}
    </p>
  );
}
