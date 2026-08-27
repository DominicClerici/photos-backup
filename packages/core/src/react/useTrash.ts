"use client";

import { useMemo } from "react";

import {
  addToAlbum,
  deleteItems,
  purgeItems,
  removeFromAlbum,
  restoreItems,
  undoDelete,
  unvault,
  vaultItems,
  type Bucket,
  type Target,
  type TimelineFilter,
  type View,
} from "../wire/api.ts";
import { counted, formatBytes, type Noun } from "../lib/format.ts";
import { closeNotice, notify, notifyError, UNDO_MS } from "../notify.ts";
import { albumsChanged } from "./useAlbums.ts";
import { BUCKET_LABEL, needsVault } from "./useVault.ts";
import type { AlbumRef, SelectionActions } from "./useSelection.tsx";

/**
 * What a grid can do to a selection, wired to one timeline.
 *
 * Everything here does the same three things in the same order: ask the server,
 * reload the timeline, say what happened. The reload is not optional and is not
 * an optimisation — a delete moves every index after it, so the day table the
 * grid was drawn from is describing a timeline that no longer exists, and
 * patching the grid in place would leave the geometry lying about what is where.
 *
 * @param filter Which timeline the positions in a selection are counted in.
 *   Undefined is the library's own. It travels to the server with every range,
 *   because index 2 in an album is not index 2 in the archive.
 * @param reload The timeline's `retry`, which refetches the day table and the
 *   pages hanging off it.
 * @param albumTitle What this album is called, when the timeline is one. Only
 *   the page knows — the filter carries a uuid — and the notice that says a
 *   photograph has left an album should say which.
 * @param view How that timeline is being sorted and filtered, which is the
 *   other half of what a position means. It travels with every range for the
 *   same reason the filter does: position 2 of a grid showing only the videos
 *   is a different photograph from position 2 of the album.
 */
export function useTrashActions(
  filter: TimelineFilter | undefined,
  reload: () => void,
  albumTitle?: string,
  view?: View,
): SelectionActions {
  return useMemo<SelectionActions>(() => {
    const scope =
      filter?.kind === "trash" ? "trash" : filter?.kind === "vault" ? "vault" : "library";
    const bucket = filter?.kind === "vault" ? filter.bucket : undefined;

    // Which album this grid *is*, if any — nested one level down inside the
    // vault, because there the bucket is the scope and the album is what it has
    // been narrowed to.
    const within =
      filter?.kind === "albums"
        ? filter.value
        : filter?.kind === "vault" && filter.within?.kind === "albums"
          ? filter.within.value
          : undefined;

    return {
      scope,
      bucket,
      album: within ? { id: within, title: albumTitle } : undefined,
      filter,
      view,

      async remove(target: Target, noun?: Noun) {
        try {
          const { batch, deleted } = await deleteItems({ ...target, filter, view });
          reload();
          // `id` is referenced by a handler defined before `notify` returns
          // it. That is safe because the handler cannot run until long after
          // this statement has finished, and it is the only way to let a notice
          // close itself.
          const id: string = notify({
            title: `${counted(deleted, noun)} deleted`,
            description: "In Recently Deleted for 365 days.",
            timeout: UNDO_MS,
            action: {
              label: "Undo",
              onPress: () => {
                // Closed at once rather than when the restore lands: the press
                // was the answer, and a notice still offering Undo while the
                // undo runs invites a second one.
                closeNotice(id);
                undoDelete(batch)
                  .then(reload)
                  .catch((err: unknown) => notifyError(err, "Could not undo"));
              },
            },
          });
        } catch (err) {
          notifyError(err, "Could not delete");
        }
      },

      // Hiding is the one write here that reaches the vault, and the only one
      // that works while it is locked. A vault that does not exist yet is not
      // an error to report — it is a password to choose — so it is handed to
      // the gate rather than to a notice. See needsVault.
      async hide(bucket: Bucket, target: Target, noun?: Noun) {
        try {
          const { batch, moved } = await vaultItems(bucket, { ...target, filter, view });
          reload();
          const id: string = notify({
            title: `${counted(moved, noun)} ${bucket === "archive" ? "archived" : "hidden"}`,
            description: `Encrypted in ${BUCKET_LABEL[bucket]}. Only your password opens it.`,
            timeout: UNDO_MS,
            action: {
              label: "Undo",
              onPress: () => {
                closeNotice(id);
                unvault({ batch })
                  .then(reload)
                  .catch((err: unknown) => notifyError(err, "Could not undo"));
              },
            },
          });
        } catch (err) {
          if (needsVault(err)) return;
          notifyError(err, `Could not ${bucket === "archive" ? "archive" : "hide"}`);
        }
      },

      // One verb, two scopes. In the trash "restore" means undeleting; in the
      // vault it means decrypting and putting back — including into the albums
      // the photographs were in, where those albums still exist.
      async restore(target: Target, noun?: Noun) {
        try {
          const restored =
            filter?.kind === "vault"
              ? (
                  await unvault({
                    bucket: filter.bucket,
                    ids: target.ids,
                    ranges: target.ranges,
                    filter: filter.within,
                    view,
                  })
                ).restored
              : (await restoreItems({ ...target, filter, view })).restored;
          reload();
          notify({
            type: "success",
            title: `${counted(restored, noun)} restored`,
            description: "Back in the library, where they were.",
          });
        } catch (err) {
          notifyError(err, "Could not restore");
        }
      },

      // Filing is not moving. Nothing leaves the library, nothing is encrypted
      // and nothing is deleted — an album is a grouping, and this is the whole
      // of what it means to be in one. So there is no batch, no undo token and
      // no reload: the timeline on screen is unchanged unless it happens to be
      // this album's own, which is the one case checked for below.
      async file(album: AlbumRef, target: Target, noun?: Noun) {
        try {
          const { added = 0 } = await addToAlbum(album.id, { ...target, filter, view }, bucket);
          albumsChanged();
          if (within === album.id) reload();
          notify(
            added > 0
              ? {
                  type: "success",
                  title: `${counted(added, noun)} added to ${named(album)}`,
                }
              : {
                  title: `Already in ${named(album)}`,
                  description: "Nothing new to add.",
                },
          );
        } catch (err) {
          if (needsVault(err)) return;
          notifyError(err, "Could not add to the album");
        }
      },

      // The undo is offered only when the request named exact ids, because that
      // is the only case where putting them back means the same photographs. A
      // removal named by position has already moved every position after it.
      async unfile(album: AlbumRef, target: Target, noun?: Noun) {
        try {
          const { removed = 0 } = await removeFromAlbum(album.id, { ...target, filter, view }, bucket);
          albumsChanged();
          reload();

          const exact = target.ids?.length ? target.ids : undefined;
          const id: string = notify({
            title: `${counted(removed, noun)} removed from ${named(album)}`,
            description: bucket
              ? `Still in ${BUCKET_LABEL[bucket]}.`
              : "Still in the library.",
            timeout: exact ? UNDO_MS : undefined,
            action: exact
              ? {
                  label: "Undo",
                  onPress: () => {
                    closeNotice(id);
                    addToAlbum(album.id, { ids: exact }, bucket)
                      .then(() => {
                        albumsChanged();
                        reload();
                      })
                      .catch((err: unknown) => notifyError(err, "Could not undo"));
                  },
                }
              : undefined,
          });
        } catch (err) {
          if (needsVault(err)) return;
          notifyError(err, "Could not remove from the album");
        }
      },

      async purge(target: Target, noun?: Noun) {
        try {
          const { purged, bytes } = await purgeItems({ ...target, filter, view });
          reload();
          notify({
            title: `${counted(purged, noun)} permanently deleted`,
            description: bytes > 0 ? `${formatBytes(bytes)} freed.` : undefined,
          });
        } catch (err) {
          notifyError(err, "Could not delete");
        }
      },
    };
  }, [filter, reload, albumTitle, view]);
}

/** What to call an album in a sentence, whether or not its name is in hand. */
function named(album: AlbumRef): string {
  return album.title ? `“${album.title}”` : "the album";
}
