"use client";

import Link from "next/link";
import { Archive, ChevronRight, EyeOff, Inbox, Lock, Trash2, type LucideIcon } from "lucide-react";

import { askToUnlock } from "@/hooks/useVault";

/**
 * The rows that are less a slice of the library than a place a photo is put:
 * out of the timeline, out of sight, or on its way out altogether.
 *
 * The row is CategoryList's, minus the cover. A category always has a
 * photograph that can stand for it — the server never sends an empty one —
 * whereas these are fixed destinations that exist whether or not anything is in
 * them, so the icon carries the row on its own.
 *
 * Four rows, and the first two are not the same thing despite both saying
 * something like "archive".
 *
 * **Archived** is Google's flag, imported. It is a category like any other — a
 * predicate over `assets.archived`, linking to the same filtered timeline every
 * other category links to — and it says what Google Photos was told, years ago,
 * about photographs that are otherwise entirely ordinary members of this
 * library. Nothing about it is encrypted and nothing about it is a decision
 * made here. It sits at the top of this list rather than in Categories because
 * it reads as a destination even though it is not one, and it stays until
 * there is somewhere better to put an imported flag.
 *
 * **Archive** is this archive's own, added in Phase 11. A photograph in it is
 * encrypted on disk, out of every album and category, and unreadable without a
 * password. The two are not related and the labels are the only thing they have
 * in common, which is why the older one keeps the past tense it was imported
 * with and the new one takes the plain noun.
 */
type Entry = {
  key: string;
  label: string;
  Icon: LucideIcon;
  href: string;
  /** True for the two rows behind the password. */
  locked?: boolean;
};

const ENTRIES: Entry[] = [
  { key: "archived", label: "Archived", Icon: Inbox, href: "/collections/categories/archived" },
  { key: "archive", label: "Archive", Icon: Archive, href: "/archive", locked: true },
  { key: "hidden", label: "Hidden", Icon: EyeOff, href: "/hidden", locked: true },
  { key: "trash", label: "Recently Deleted", Icon: Trash2, href: "/trash" },
];

/**
 * @param counts How much is in each entry, by key. A key with no count draws no
 * number rather than a zero, because "not counted yet" and "empty" are
 * different things and only one of them is worth saying.
 *
 * The vault's two rows exercise that distinction for a stronger reason than
 * the others: while the vault is locked the server does not send those counts
 * at all. How much somebody has hidden is a fact about what they hid, and a row
 * reading "Hidden — 41" would give it away to anyone who walked past the
 * screen. Locked, the row says so and nothing else.
 *
 * @param unlocked Whether the vault is open. Only changes what the two vault
 * rows say; the destination is the same either way, and the page behind it does
 * the asking.
 */
export function OtherList({
  counts,
  unlocked,
}: {
  counts?: Record<string, number | undefined>;
  unlocked?: boolean;
}) {
  return (
    <ul className="overflow-hidden rounded-xl border bg-card">
      {ENTRIES.map(({ key, label, Icon, href, locked }, i) => {
        const shut = locked && !unlocked;
        const count = counts?.[key];

        return (
          <li key={key}>
            <Link
              href={href}
              // A locked row is a link like any other — the page behind it is
              // the right place to ask for the password, because that is where
              // somebody can see what they are unlocking. Clicking it opens the
              // prompt on the way, so the common case is one click rather than
              // one click and then another on an empty page.
              onClick={() => shut && askToUnlock()}
              className={
                "flex h-16 items-center gap-3.5 px-3.5 transition-colors hover:bg-foreground/[0.04] focus-visible:outline-2 -outline-offset-2 focus-visible:outline-ring" +
                (i > 0 ? " border-t" : "")
              }
            >
              <span className="flex size-11 shrink-0 items-center justify-center rounded-lg bg-tile">
                <Icon className="size-[18px] text-foreground" aria-hidden="true" />
              </span>

              <span className="flex-1 truncate text-sm font-medium">{label}</span>

              {shut ? (
                <span className="flex items-center gap-1.5 text-[13px] text-faint">
                  <Lock className="size-3.5" aria-hidden="true" />
                  Locked
                </span>
              ) : count === undefined ? null : (
                <span className="text-[13px] text-faint">{count.toLocaleString()}</span>
              )}

              <ChevronRight className="size-4 shrink-0 text-faint" aria-hidden="true" />
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
