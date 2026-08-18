import { notFound } from "next/navigation";

import { VaultTimelineView } from "@/components/VaultTimelineView";
import type { CollectionFilter } from "@/lib/api";

/**
 * One collection inside Hidden: /hidden/albums/<uuid>,
 * /hidden/people/<name>, /hidden/categories/<key>.
 *
 * Deliberately the same three kinds and the same shape as
 * /collections/[kind]/[value]. A hidden photograph is still in the albums it
 * was in and still has the people in it that it had — all of that went into the
 * sealed document with it — so a bucket has real collections rather than a flat
 * pile, and browsing them is the same act with the same URL grammar.
 */
const KINDS: CollectionFilter["kind"][] = ["albums", "people", "categories"];

export default async function Page({
  params,
}: {
  params: Promise<{ kind: string; value: string }>;
}) {
  const { kind, value } = await params;
  if (!KINDS.includes(kind as CollectionFilter["kind"])) notFound();

  return (
    <VaultTimelineView
      bucket="hidden"
      within={{ kind, value } as CollectionFilter}
    />
  );
}
