"use client";

import { useCallback, useState } from "react";
import Link from "next/link";
import { Archive, EyeOff, Images, RotateCcw, Trash2 } from "lucide-react";

import { deleteAlbum, unvault, undoDelete, vaultAlbum, type Album, type Bucket } from "@/lib/api";
import { albumsChanged } from "@/hooks/useAlbums";
import { counted, describeAction } from "@/lib/format";
import { BUCKET_LABEL, BUCKET_VERB, needsVault } from "@/hooks/useVault";
import { notifyError, UNDO_MS } from "@/lib/notify";
import { toast } from "@/components/ui/toast";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuLabel,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu";
import { ArmedMenuItem } from "./Armed";
import { Cover } from "./Cover";

/**
 * The albums, the one holding the most recent photo first.
 *
 * An album with nothing in it is still drawn. Those come from an import whose
 * directory produced no assets, and one that quietly vanished would be a fact
 * about a failed import that nobody ever finds out.
 *
 * @param onChanged Called after a delete lands, and after one is undone, so the
 * page can re-read the index. There is nothing to patch in place here: an album
 * is a row in a list, and the list is one request.
 */
export function AlbumGrid({
  albums,
  onChanged,
  basePath = "/collections/albums",
  bucket,
}: {
  albums: Album[];
  onChanged?: () => void;
  /** Where a tile leads. The library's albums, or a bucket's. */
  basePath?: string;
  /**
   * Set when this grid is being drawn inside a bucket, which flips the menu
   * from three ways of putting an album away to the one way of getting it back.
   * The tiles themselves are identical, which is why this is a prop and not a
   * second component.
   */
  bucket?: Bucket;
}) {
  return (
    <ul className="grid grid-cols-2 gap-x-4 gap-y-5 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5">
      {albums.map((album) => (
        <AlbumTile
          key={album.id}
          album={album}
          onChanged={onChanged}
          basePath={basePath}
          inBucket={bucket}
        />
      ))}
    </ul>
  );
}

/**
 * One album, and the two ways of deleting it.
 *
 * They are two menu items rather than one with a checkbox because they are two
 * different acts and only one of them is about photographs. Dropping the album
 * leaves every picture in it exactly where it was in the library — it is the
 * grouping an import produced that goes. Dropping the album *and* its contents
 * is a delete of forty photographs that happens to be spelled as an album, and
 * it should have to be aimed at deliberately.
 *
 * A menu per album rather than one for the grid: albums are counted in tens
 * here, where the timeline's tiles are counted in thousands and share a single
 * menu for that reason.
 */
function AlbumTile({
  album,
  onChanged,
  basePath,
  inBucket,
}: {
  album: Album;
  onChanged?: () => void;
  basePath: string;
  inBucket?: Bucket;
}) {
  const [open, setOpen] = useState(false);

  // Every write below ends in this, so the menus that list albums are told at
  // the same moment the page is. See useAlbums.
  const changed = useCallback(() => {
    albumsChanged();
    onChanged?.();
  }, [onChanged]);

  const bringBack = useCallback(() => {
    if (!inBucket) return;
    unvault({ bucket: inBucket, album: album.id })
      .then(({ restored }) => {
        changed();
        toast.add({
          type: "success",
          title: `“${album.title}” restored`,
          description: `${counted(restored)} back in the library, in the album.`,
        });
      })
      .catch((err: unknown) => {
        if (needsVault(err)) return;
        notifyError(err, "Could not restore the album");
      });
  }, [inBucket, album.id, album.title, changed]);

  const remove = useCallback(
    (photos: boolean) => {
      deleteAlbum(album.id, photos)
        .then(({ batch, deleted }) => {
          changed();
          // Referenced by the handler below, which cannot run until long after
          // this statement has finished. See useTrash.
          const id: string = toast.add({
            title: photos
              ? `Album and ${counted(deleted)} deleted`
              : `“${album.title}” deleted`,
            description: photos
              ? "The photos are in Recently Deleted for 365 days."
              : "The photos in it are still in the library.",
            timeout: UNDO_MS,
            actionProps: {
              children: "Undo",
              onClick: () => {
                toast.close(id);
                undoDelete(batch)
                  .then(() => changed())
                  .catch((err: unknown) => notifyError(err, "Could not undo"));
              },
            },
          });
        })
        .catch((err: unknown) => notifyError(err, "Could not delete the album"));
    },
    [album.id, album.title, changed],
  );

  // "Archive album", not "Archive Iceland 2025". The album is called by what it
  // is rather than by its name, because a title in a verb phrase reads like a
  // place: "Archive Iceland 2025" is a sentence about Iceland. A person is the
  // other way round, and PeopleRow says so.
  const fileAway = useCallback(
    (bucket: Bucket) => {
      vaultAlbum(bucket, album.id)
        .then(({ batch, moved }) => {
          changed();
          const id: string = toast.add({
            title: `“${album.title}” ${bucket === "archive" ? "archived" : "hidden"}`,
            description: `${counted(moved)} encrypted in ${BUCKET_LABEL[bucket]}, with the album.`,
            timeout: UNDO_MS,
            actionProps: {
              children: "Undo",
              onClick: () => {
                toast.close(id);
                unvault({ batch })
                  .then(() => changed())
                  .catch((err: unknown) => notifyError(err, "Could not undo"));
              },
            },
          });
        })
        .catch((err: unknown) => {
          if (needsVault(err)) return;
          notifyError(err, `Could not ${BUCKET_VERB[bucket].toLowerCase()} the album`);
        });
    },
    [album.id, album.title, changed],
  );

  return (
    <li>
      <ContextMenu onOpenChange={setOpen}>
        <ContextMenuTrigger
          // `contents` so the trigger is a listener rather than a box: the grid
          // cell is the <li>, and a second element inside it would change what
          // the cover is sized against.
          className="contents"
        >
          <Link
            href={`${basePath}/${album.id}`}
            className="group block rounded-xl focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
          >
            <Cover
              id={album.cover_id}
              className="aspect-square w-full rounded-xl ring-1 ring-border transition-[scale] duration-200 ease-out group-hover:scale-[1.02] motion-reduce:transition-none"
            />
            <p className="mt-2 truncate text-[13px] font-medium" title={album.title}>
              {album.title}
            </p>
            <p className="text-xs text-faint">
              {album.count.toLocaleString()} {album.count === 1 ? "item" : "items"}
            </p>
          </Link>
        </ContextMenuTrigger>

        <ContextMenuContent className="min-w-56">
          <ContextMenuLabel className="truncate">{album.title}</ContextMenuLabel>
          <ContextMenuSeparator />

          {inBucket ? (
            // One item, and no delete. Taking an album out of a bucket and then
            // deleting it is two decisions; a single button that decrypted an
            // album in order to throw it away would be spending the password on
            // the one operation that does not need it.
            <ContextMenuItem onClick={bringBack}>
              <RotateCcw />
              {describeAction(inBucket === "hidden" ? "Unhide" : "Unarchive", { kind: "album" })}
            </ContextMenuItem>
          ) : (
            <>
              {/* Both of these take the photographs with them, which is the
                  whole difference from "Delete album" below: hiding a grouping
                  that left its contents in plain sight would not be hiding
                  anything. */}
              <ContextMenuItem onClick={() => fileAway("archive")}>
                <Archive />
                {describeAction("Archive", { kind: "album" })}
              </ContextMenuItem>
              <ContextMenuItem onClick={() => fileAway("hidden")}>
                <EyeOff />
                {describeAction("Hide", { kind: "album" })}
              </ContextMenuItem>

              <ContextMenuSeparator />

              <ArmedMenuItem
                label={describeAction("Delete", { kind: "album" })}
                icon={<Images />}
                onConfirm={() => remove(false)}
                open={open}
              />
              <ArmedMenuItem
                label="Delete album and photos"
                icon={<Trash2 />}
                onConfirm={() => remove(true)}
                open={open}
                disabled={album.count === 0}
              />
            </>
          )}
        </ContextMenuContent>
      </ContextMenu>
    </li>
  );
}
