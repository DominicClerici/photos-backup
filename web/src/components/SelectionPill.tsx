"use client";

import { useCallback, useEffect, useState } from "react";
import { Archive, ChevronUp, CircleCheck, EyeOff, ListMinus, RotateCcw, Trash2 } from "lucide-react";

import { useSelection, type AlbumRef, type SelectionActions } from "@/hooks/useSelection";
import type { Bucket, CreatedAlbum } from "@/lib/api";
import { counted, describeAction } from "@/lib/format";
import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogMedia,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { toast } from "@/components/ui/toast";
import { ArmedButton } from "./Armed";
import { AddToAlbumMenu } from "./AlbumPicker";
import { CreateAlbumDialog, type CreateAlbumRequest } from "./CreateAlbumDialog";
import { cn } from "@/lib/utils";

/**
 * The selection control, standing to the left of the tab bar.
 *
 * A circle until there is a selection, then a pill with a count in it. It sits
 * in a row anchored by its right edge a few pixels from the tab bar and growing
 * leftwards, so that turning selection mode on never shoves the tabs across the
 * screen — the one thing on this bar that must not move, because it is the
 * thing people aim at without looking. See TabBar.
 *
 * It draws nothing at all unless a grid is on screen to select from, which is
 * what keeps it off the collections and overview pages.
 */
export function SelectionPill() {
  const { active, count, ranges, grid, actions, sheet, setSheet, enter, exit } = useSelection();
  const [asking, setAsking] = useState(false);
  const [creating, setCreating] = useState<CreateAlbumRequest | null>(null);

  // Whatever the destructive action is here — the library's delete, the trash's
  // purge — pointed at the whole selection.
  //
  // The vault has neither, so there is nothing to point: a selection there can
  // come back out and that is all. Guarding here rather than only hiding the
  // button is what keeps the Delete key from falling through to the library's
  // delete, where the positions in this selection mean other photographs.
  const destroy = useCallback(() => {
    if (!actions || count === 0 || actions.scope === "vault") return;
    const target = { ranges };
    void (actions.scope === "trash" ? actions.purge(target) : actions.remove(target));
    exit();
  }, [actions, count, ranges, exit]);

  const restore = useCallback(() => {
    if (!actions || count === 0) return;
    void actions.restore({ ranges });
    exit();
  }, [actions, count, ranges, exit]);

  const fileAway = useCallback(
    (bucket: Bucket) => {
      if (!actions || count === 0) return;
      void actions.hide(bucket, { ranges });
      exit();
    },
    [actions, count, ranges, exit],
  );

  const fileInto = useCallback(
    (album: AlbumRef) => {
      if (!actions || count === 0) return;
      void actions.file(album, { ranges });
      exit();
    },
    [actions, count, ranges, exit],
  );

  const unfileFrom = useCallback(
    (album: AlbumRef) => {
      if (!actions || count === 0) return;
      void actions.unfile(album, { ranges });
      exit();
    },
    [actions, count, ranges, exit],
  );

  // The runs are captured now rather than read when the dialog is submitted:
  // typing a name takes long enough that the selection may have changed, and
  // every position in it would then mean a different photograph.
  const startCreate = useCallback(
    (name: string) => {
      if (!actions || count === 0) return;
      setCreating({
        name,
        bucket: actions.bucket,
        target: { ranges, filter: actions.filter, view: actions.view },
      });
    },
    [actions, count, ranges],
  );

  const created = useCallback(
    (album: CreatedAlbum) => {
      toast.add({
        type: "success",
        title: `“${album.title}” created`,
        description: `${counted(album.added ?? 0)} in it.`,
      });
      exit();
    },
    [exit],
  );

  // Delete and Backspace, the keys this gesture has meant on every desktop for
  // thirty years. They ask before acting: a keystroke has no armed state to
  // pass through, so the dialog is where the second decision goes. Escape
  // leaving selection mode is the provider's, one layer up.
  useEffect(() => {
    if (!active || count === 0 || !actions || actions.scope === "vault") return;

    const onKey = (ev: KeyboardEvent) => {
      if (ev.key !== "Delete" && ev.key !== "Backspace") return;
      // Not while somebody is typing, and not while a dialog is already up.
      const el = document.activeElement;
      if (el instanceof HTMLElement && (el.isContentEditable || /^(INPUT|TEXTAREA)$/.test(el.tagName))) {
        return;
      }
      ev.preventDefault();
      setAsking(true);
    };

    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [active, count, actions]);

  // A selection that has emptied out has nothing left to confirm.
  useEffect(() => {
    if (count === 0) setAsking(false);
  }, [count]);

  if (!grid) return null;

  const forever = actions?.scope === "trash";

  return (
    <div className="pointer-events-auto relative flex flex-col items-end">
      <Sheet
        open={sheet}
        count={count}
        actions={actions}
        onDestroy={destroy}
        onRestore={restore}
        onHide={fileAway}
        onFile={fileInto}
        onUnfile={unfileFrom}
        onCreate={startCreate}
        onDone={exit}
      />

      <button
        type="button"
        onClick={() => (active ? setSheet(!sheet) : enter())}
        aria-label={active ? (sheet ? "Hide actions" : "Show actions") : "Select photos"}
        aria-expanded={active ? sheet : undefined}
        className={cn(
          "flex h-13 items-center rounded-full border bg-card/80 px-[17px] shadow-lg backdrop-blur-xl transition-colors duration-200 focus-visible:ring-2 focus-visible:ring-ring/70 focus-visible:outline-none",
          active ? "text-foreground" : "text-muted-foreground hover:text-foreground",
        )}
      >
        {/* Both glyphs are always here, one over the other, so the swap is a
            dissolve rather than a jump — and the chevron can turn over on its
            own axis when the sheet opens instead of being replaced. */}
        <span className="relative size-[18px] shrink-0">
          <CircleCheck
            className={cn(
              "absolute inset-0 size-[18px] transition-[opacity,transform] duration-200 ease-out",
              active ? "scale-50 opacity-0" : "scale-100 opacity-100",
            )}
            aria-hidden="true"
          />
          <ChevronUp
            className={cn(
              "absolute inset-0 size-[18px] transition-[opacity,transform] duration-200 ease-out",
              active ? "scale-100 opacity-100" : "scale-50 opacity-0",
              sheet && "rotate-180",
            )}
            aria-hidden="true"
          />
        </span>

        {/* A column that is 0fr wide collapsed and 1fr open: the label keeps its
            own width throughout and the button's width follows it, which is
            what makes the growth animate at all — `width: auto` does not. */}
        <span
          className="grid transition-[grid-template-columns] duration-300 ease-[cubic-bezier(0.22,1,0.36,1)] motion-reduce:transition-none"
          style={{ gridTemplateColumns: active ? "1fr" : "0fr" }}
        >
          <span className="overflow-hidden">
            <span
              className={cn(
                "block pl-2.5 text-[13px] font-medium tracking-[0.01em] whitespace-nowrap tabular-nums transition-opacity duration-200",
                // Waits for the pill to be most of the way open, so the text
                // arrives in a space that is already there for it.
                active ? "opacity-100 delay-100" : "opacity-0",
              )}
            >
              {count.toLocaleString()} selected
            </span>
          </span>
        </span>
      </button>

      <AlertDialog open={asking} onOpenChange={setAsking}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogMedia>
              <Trash2 className="text-destructive" aria-hidden="true" />
            </AlertDialogMedia>
            <AlertDialogTitle>
              {forever ? "Permanently delete" : "Delete"} {counted(count)}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {forever
                ? "The originals and everything made from them are removed from the archive. This cannot be undone."
                : "They move to Recently Deleted and stay there for 365 days."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-background hover:bg-destructive/90 focus-visible:ring-destructive/40"
              onClick={() => {
                setAsking(false);
                destroy();
              }}
            >
              {forever ? "Delete forever" : "Delete"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <CreateAlbumDialog
        request={creating}
        onClose={() => setCreating(null)}
        onCreated={created}
      />
    </div>
  );
}

/**
 * What can be done to the selection, above the pill.
 *
 * Which actions these are is the grid's to say rather than this component's:
 * the same gesture means "delete" in the library and "restore or destroy" in
 * Recently Deleted, and the pill is mounted by the root layout and has no idea
 * which one is on screen. See SelectionActions.
 *
 * Every destructive control here is armed rather than confirmed in a dialog —
 * see ArmedButton. The keyboard's Delete gets the dialog instead, because a
 * keystroke has no first click to spend.
 */
function Sheet({
  open,
  count,
  actions,
  onDestroy,
  onRestore,
  onHide,
  onFile,
  onUnfile,
  onCreate,
  onDone,
}: {
  open: boolean;
  count: number;
  actions: SelectionActions | null;
  onDestroy: () => void;
  onRestore: () => void;
  onHide: (bucket: Bucket) => void;
  onFile: (album: AlbumRef) => void;
  onUnfile: (album: AlbumRef) => void;
  onCreate: (name: string) => void;
  onDone: () => void;
}) {
  const trash = actions?.scope === "trash";
  const vault = actions?.scope === "vault";
  const album = trash ? undefined : actions?.album;
  const empty = count === 0;
  // The sheet has no tile under a pointer to ask what kind of thing this is, so
  // it says "items" from two upwards and falls back to the generic noun at one.
  // The menu, which does know, says "photo" or "video". See describeAction.
  const about = { kind: "items", count } as const;

  return (
    <div
      className={cn(
        // Out of the flow, not merely hidden. Closed it is still 240px wide,
        // and in the row of floating controls that width would sit between the
        // pill and its neighbour as a gap nothing is drawn in.
        "absolute right-0 bottom-full mb-2 w-60 rounded-2xl border bg-card/80 p-3 shadow-lg backdrop-blur-xl transition-[opacity,transform] duration-200 ease-out motion-reduce:transition-none",
        open ? "translate-y-0 opacity-100" : "pointer-events-none invisible translate-y-2 opacity-0",
      )}
      // Not merely invisible: a panel nobody can see should not be reachable by
      // Tab either, and `visibility` is what takes it out of the tab order.
      aria-hidden={!open}
    >
      {empty ? (
        <p className="text-xs leading-relaxed text-faint">
          Nothing selected yet. Pick photos to act on them.
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {trash || vault ? (
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="w-full justify-start gap-2"
              onClick={onRestore}
            >
              <RotateCcw aria-hidden="true" />
              {vault
                ? describeAction(actions?.bucket === "hidden" ? "Unhide" : "Unarchive", about)
                : "Restore"}
            </Button>
          ) : null}

          {/* No ticks here, and not for want of trying: the sheet knows the
              selection as runs of positions and has never fetched the
              photographs in them, so "which albums is this one already in" is a
              question it cannot ask. The grid's own menu can, and does. */}
          {trash || !actions ? null : (
            <AddToAlbumMenu
              bucket={actions?.bucket}
              assetId={null}
              onAdd={onFile}
              onCreate={onCreate}
            />
          )}

          {/* Not armed, and not last. Filing something away is undoable from
              the toast and reversible from a page one tap from here; only the
              delete below is buying two clicks with the thing it cannot buy
              back. Putting them in this order is the whole of what says so. */}
          {!trash && !vault ? (
            <>
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="w-full justify-start gap-2"
                onClick={() => onHide("archive")}
              >
                <Archive aria-hidden="true" />
                {describeAction("Archive", about)}
              </Button>
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="w-full justify-start gap-2"
                onClick={() => onHide("hidden")}
              >
                <EyeOff aria-hidden="true" />
                {describeAction("Hide", about)}
              </Button>
            </>
          ) : null}

          {album ? (
            <ArmedButton
              label={`${describeAction("Remove", about)} from album`}
              icon={<ListMinus aria-hidden="true" />}
              tone="neutral"
              onConfirm={() => onUnfile(album)}
              open={open}
            />
          ) : null}

          {vault ? null : (
            <ArmedButton
              label={describeAction(trash ? "Delete forever" : "Delete", about)}
              icon={<Trash2 aria-hidden="true" />}
              onConfirm={onDestroy}
              open={open}
              disabled={!actions}
            />
          )}

          <p className="px-0.5 text-[11px] leading-relaxed text-faint">
            {trash
              ? "Removes the originals from the archive. There is no undo."
              : vault
                ? "Decrypted and put back where they were, including any album that still exists."
                : `${counted(count)} to Recently Deleted, for 365 days.`}
          </p>
        </div>
      )}

      <Button variant="outline" size="sm" className="mt-3 w-full" onClick={onDone}>
        Done
      </Button>
    </div>
  );
}
