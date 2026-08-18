"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import {
  ArrowLeft,
  ChevronRight,
  Images,
  Loader2,
  Lock,
  LockOpen,
  ShieldCheck,
  type LucideIcon,
} from "lucide-react";

import { useRouter } from "next/navigation";

import {
  fetchVaultCollections,
  type Bucket,
  type CreatedAlbum,
  type VaultCollections,
} from "@/lib/api";
import { albumsChanged } from "@/hooks/useAlbums";
import { askToUnlock, BUCKET_LABEL, needsVault, useVault } from "@/hooks/useVault";
import { Button } from "@/components/ui/button";
import { AlbumGrid } from "./AlbumGrid";
import { NewAlbumButton } from "./Collections";
import { CreateAlbumDialog, type CreateAlbumRequest } from "./CreateAlbumDialog";
import { CategoryList } from "./CategoryList";
import { PeopleRow } from "./PeopleRow";

/**
 * One bucket's front page: the collections page, over what is inside the vault.
 *
 * Deliberately the same three sections in the same order, drawn by the same
 * three components. A hidden photograph is still in the albums it was in and
 * still has the people in it that it had — that went into the sealed document
 * with everything else — so there is a real collections page in here, and
 * inventing a second, flatter way of browsing it would be inventing a worse one.
 *
 * What is different is the row above them and the state before them. The row is
 * everything in the bucket, as one timeline, because unlike the library this is
 * small enough that "all of it" is a reasonable thing to open. The state is the
 * lock: until the password has been typed this page knows nothing at all — not
 * the albums, not the count, not a single thumbnail — because the server will
 * not tell it.
 */
export function VaultView({ bucket }: { bucket: Bucket }) {
  const vault = useVault();
  const [data, setData] = useState<VaultCollections | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [attempt, retry] = useState(0);
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);
  const router = useRouter();

  const unlocked = vault.status?.unlocked === true;

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
        setError(err instanceof Error ? err.message : "could not open the vault");
      });
    return () => abort.abort();
  }, [bucket, unlocked, attempt]);

  // Arriving at a locked page asks straight away rather than making somebody
  // find the button: there is one thing to do here and it is the same thing
  // every time.
  useEffect(() => {
    if (vault.ready && !unlocked) askToUnlock(vault.status);
  }, [vault.ready, vault.status, unlocked]);

  const reload = useCallback(() => retry((n) => n + 1), []);

  // An album made in here is an archived album from the moment it exists: there
  // is nothing in it to move, and the alternative — make it in the library and
  // then hide it — would put its title on the collections page in between.
  const created = useCallback(
    (album: CreatedAlbum) => {
      albumsChanged();
      router.push(`/${bucket}/albums/${album.id}`);
    },
    [bucket, router],
  );

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-2 sm:px-4">
        <Link
          href="/collections"
          aria-label="Back to collections"
          className="flex size-9 shrink-0 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/[0.05] hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          <ArrowLeft className="size-[18px]" aria-hidden="true" />
        </Link>

        <h1 className="truncate text-[15px] font-semibold tracking-[0.01em]">
          {BUCKET_LABEL[bucket]}
        </h1>
        {data ? (
          <span className="shrink-0 text-[13px] text-faint">
            {data.total.toLocaleString()} items
          </span>
        ) : null}

        <div className="ml-auto flex shrink-0 items-center gap-2">
          {unlocked ? (
            <Button variant="outline" size="sm" onClick={() => void vault.lock()}>
              <Lock aria-hidden="true" />
              Lock
            </Button>
          ) : (
            <Button variant="outline" size="sm" onClick={() => askToUnlock(vault.status)}>
              <LockOpen aria-hidden="true" />
              Unlock
            </Button>
          )}
        </div>
      </header>

      <div className="h-full overflow-x-hidden overflow-y-auto overscroll-y-contain px-4 pb-28">
        {!vault.ready ? (
          <Notice>
            <Loader2 className="size-4 animate-spin" aria-hidden="true" />
            Checking the vault
          </Notice>
        ) : !unlocked ? (
          <Locked bucket={bucket} exists={vault.status?.exists === true} />
        ) : error ? (
          <Notice>
            <span className="text-destructive">{error}</span>
            <Button variant="outline" size="sm" onClick={reload}>
              Try again
            </Button>
          </Notice>
        ) : !data ? (
          <Notice>
            <Loader2 className="size-4 animate-spin" aria-hidden="true" />
            Opening {BUCKET_LABEL[bucket]}
          </Notice>
        ) : (
          <div className="mx-auto max-w-7xl">
            <Section title="Everything">
              <DestinationRow
                href={`/${bucket}/all`}
                label={`All of ${BUCKET_LABEL[bucket]}`}
                Icon={Images}
                count={data.total}
              />
            </Section>

            <Section title="People" count={data.people.length}>
              <PeopleRow
                people={data.people}
                onChanged={reload}
                basePath={`/${bucket}/people`}
                bucket={bucket}
              />
            </Section>

            {/* The same three components as the collections page, given the
                vault's routes and told which bucket they are in. A grouping
                already in a bucket has one thing that can be done to it, so
                their menus offer Unarchive or Unhide where the library's offer
                Archive, Hide and Delete. */}
            <Section
              title="Albums"
              count={data.albums.length}
              action={<NewAlbumButton onClick={() => setCreating({ name: "", bucket })} />}
            >
              <AlbumGrid
                albums={data.albums}
                onChanged={reload}
                basePath={`/${bucket}/albums`}
                bucket={bucket}
              />
            </Section>

            <Section title="Categories" count={data.categories.length}>
              <CategoryList categories={data.categories} basePath={`/${bucket}/categories`} />
            </Section>

            {data.total === 0 ? (
              <Notice>
                Nothing in {BUCKET_LABEL[bucket]} yet. Right-click a photo, an
                album or a person to put it here.
              </Notice>
            ) : null}
          </div>
        )}
      </div>

      <CreateAlbumDialog
        request={creating}
        onClose={() => setCreating(null)}
        onCreated={created}
      />
    </div>
  );
}

/**
 * What the page says with no password in hand.
 *
 * It says nothing about the contents, because it knows nothing about them: the
 * counts, the albums and the thumbnails all come from an endpoint that answers
 * 423 until this is dealt with. That is the whole promise of the feature stated
 * as a screen — there is no partial view, no blurred grid, no "41 items" to
 * read over somebody's shoulder.
 */
function Locked({ bucket, exists }: { bucket: Bucket; exists: boolean }) {
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-4 px-6 py-24 text-center">
      <span className="flex size-14 items-center justify-center rounded-2xl bg-tile">
        <ShieldCheck className="size-6 text-muted-foreground" aria-hidden="true" />
      </span>
      <h2 className="text-[15px] font-semibold">{BUCKET_LABEL[bucket]} is locked</h2>
      <p className="text-[13px] leading-relaxed text-muted-foreground">
        {exists
          ? "The photos and videos in here are encrypted on the drive. Nothing about them — not a thumbnail, not a count — is readable until the vault is unlocked."
          : "Nothing has been archived or hidden yet. Choose a password and this becomes an encrypted corner of the archive that only that password opens."}
      </p>
      <Button onClick={() => askToUnlock(exists ? { exists, unlocked: false } : undefined)}>
        <LockOpen aria-hidden="true" />
        {exists ? "Unlock" : "Choose a password"}
      </Button>
    </div>
  );
}

/** The row that leads to a whole bucket as one timeline. CategoryList's, without a cover. */
function DestinationRow({
  href,
  label,
  Icon,
  count,
}: {
  href: string;
  label: string;
  Icon: LucideIcon;
  count: number;
}) {
  return (
    <ul className="overflow-hidden rounded-xl border bg-card">
      <li>
        <Link
          href={href}
          className="flex h-16 items-center gap-3.5 px-3.5 transition-colors hover:bg-foreground/[0.04] focus-visible:outline-2 -outline-offset-2 focus-visible:outline-ring"
        >
          <span className="flex size-11 shrink-0 items-center justify-center rounded-lg bg-tile">
            <Icon className="size-[18px] text-foreground" aria-hidden="true" />
          </span>
          <span className="flex-1 truncate text-sm font-medium">{label}</span>
          <span className="text-[13px] text-faint">{count.toLocaleString()}</span>
          <ChevronRight className="size-4 shrink-0 text-faint" aria-hidden="true" />
        </Link>
      </li>
    </ul>
  );
}

/** Collections' own Section, copied rather than shared: see the note there. */
function Section({
  title,
  count,
  action,
  children,
}: {
  title: string;
  count?: number;
  action?: React.ReactNode;
  children: React.ReactNode;
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

function Notice({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex items-center justify-center gap-3 px-3 py-7 text-sm text-muted-foreground">
      {children}
    </p>
  );
}
