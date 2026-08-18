"use client";

import { useCallback } from "react";
import Link from "next/link";
import { Archive, EyeOff, RotateCcw } from "lucide-react";

import { unvault, vaultPerson, type Bucket, type Person } from "@/lib/api";
import { counted, describeAction } from "@/lib/format";
import { BASE_THUMB_SIZE } from "@/lib/layout";
import { notifyError, UNDO_MS } from "@/lib/notify";
import { BUCKET_LABEL, BUCKET_VERB, needsVault } from "@/hooks/useVault";
import { ScrollArea } from "@/components/ui/scroll-area";
import { toast } from "@/components/ui/toast";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuLabel,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu";
import { Cover } from "./Cover";

/**
 * The tagged names, most photographed first.
 *
 * The circles are photographs someone appears in, not faces: nothing in this
 * archive knows where in a frame a person is, so cropping to one would be
 * guessing. It reads as a face row anyway at this size, and it stops being a
 * lie the day the v2 face work gives it something better to draw.
 *
 * @param onChanged Called after somebody is hidden, and after that is undone,
 * so the page can re-read the index — a person is a row in a list and the list
 * is one request.
 */
export function PeopleRow({
  people,
  onChanged,
  basePath = "/collections/people",
  bucket,
}: {
  people: Person[];
  onChanged?: () => void;
  /** Where a circle leads. The library's people, or a bucket's. */
  basePath?: string;
  /**
   * Set when this row is being drawn inside a bucket, which flips the menu from
   * "put this person away" to "bring them back". One component either way,
   * because the circles, the ordering and the covers are identical — only the
   * verb changes.
   */
  bucket?: Bucket;
}) {
  return (
    <ScrollArea orientation="horizontal" className="-mx-1">
      <ul className="flex gap-4 px-1 pb-3">
        {people.map((person) => (
          <PersonCircle
            key={person.name}
            person={person}
            onChanged={onChanged}
            basePath={basePath}
            inBucket={bucket}
          />
        ))}
      </ul>
    </ScrollArea>
  );
}

/**
 * One name, and the two places it can be put.
 *
 * There is no Delete here and there never was: a person is a label an import
 * carried, not a thing this archive owns, and "delete Brody" has no coherent
 * meaning short of deleting every photograph of them. Hiding does — it means
 * exactly "these photographs, and the fact that this name was on them" — which
 * is why this menu exists now and did not before.
 *
 * The label is the name, unlike an album's. "Hide Brody" is what somebody
 * means; "Hide person" would be asking them to remember which circle they had
 * right-clicked.
 */
function PersonCircle({
  person,
  onChanged,
  basePath,
  inBucket,
}: {
  person: Person;
  onChanged?: () => void;
  basePath: string;
  inBucket?: Bucket;
}) {
  const bringBack = useCallback(() => {
    if (!inBucket) return;
    unvault({ bucket: inBucket, person: person.name })
      .then(({ restored }) => {
        onChanged?.();
        toast.add({
          type: "success",
          title: `${person.name} restored`,
          description: `${counted(restored)} back in the library, where they were.`,
        });
      })
      .catch((err: unknown) => {
        if (needsVault(err)) return;
        notifyError(err, `Could not restore ${person.name}`);
      });
  }, [inBucket, person.name, onChanged]);

  const fileAway = useCallback(
    (bucket: Bucket) => {
      vaultPerson(bucket, person.name)
        .then(({ batch, moved }) => {
          onChanged?.();
          const id: string = toast.add({
            title: `${person.name} ${bucket === "archive" ? "archived" : "hidden"}`,
            description: `${counted(moved)} encrypted in ${BUCKET_LABEL[bucket]}.`,
            timeout: UNDO_MS,
            actionProps: {
              children: "Undo",
              onClick: () => {
                toast.close(id);
                unvault({ batch })
                  .then(() => onChanged?.())
                  .catch((err: unknown) => notifyError(err, "Could not undo"));
              },
            },
          });
        })
        .catch((err: unknown) => {
          if (needsVault(err)) return;
          notifyError(err, `Could not ${BUCKET_VERB[bucket].toLowerCase()} ${person.name}`);
        });
    },
    [person.name, onChanged],
  );

  return (
    <li>
      <ContextMenu>
        {/* `contents` so the trigger is a listener rather than a box: the row's
            geometry is the <li>, and a second element inside it would change
            what the circle is sized against. */}
        <ContextMenuTrigger className="contents">
          <Link
            href={`${basePath}/${encodeURIComponent(person.name)}`}
            className="group flex w-20 flex-col items-center gap-2 rounded-lg focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
          >
            <Cover
              id={person.cover_id}
              size={BASE_THUMB_SIZE}
              className="size-20 rounded-full ring-1 ring-border transition-[scale] duration-200 ease-out group-hover:scale-[1.04] motion-reduce:transition-none"
            />
            <span className="w-full truncate text-center text-[13px] text-muted-foreground transition-colors group-hover:text-foreground">
              {person.name}
            </span>
          </Link>
        </ContextMenuTrigger>

        <ContextMenuContent className="min-w-56">
          <ContextMenuLabel className="truncate">
            {person.name} · {counted(person.count)}
          </ContextMenuLabel>
          <ContextMenuSeparator />

          {inBucket ? (
            <ContextMenuItem onClick={bringBack}>
              <RotateCcw />
              {describeAction(inBucket === "hidden" ? "Unhide" : "Unarchive", {
                kind: "person",
                name: person.name,
              })}
            </ContextMenuItem>
          ) : (
            <>
              <ContextMenuItem onClick={() => fileAway("archive")}>
                <Archive />
                {describeAction("Archive", { kind: "person", name: person.name })}
              </ContextMenuItem>
              <ContextMenuItem onClick={() => fileAway("hidden")}>
                <EyeOff />
                {describeAction("Hide", { kind: "person", name: person.name })}
              </ContextMenuItem>
            </>
          )}
        </ContextMenuContent>
      </ContextMenu>
    </li>
  );
}
