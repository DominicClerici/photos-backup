import { notFound } from "next/navigation";

import { CollectionView } from "@/components/CollectionView";
import type { CollectionFilter } from "@/lib/api";

/**
 * One collection: /collections/albums/<uuid>, /collections/people/<name>, or
 * /collections/categories/<key>.
 *
 * One route rather than three because all three do the same thing — browse a
 * filtered timeline — and the kind is only ever the query parameter to send.
 */
const KINDS: CollectionFilter["kind"][] = ["albums", "people", "categories"];

export default async function Page({
  params,
}: {
  params: Promise<{ kind: string; value: string }>;
}) {
  const { kind, value } = await params;
  if (!KINDS.includes(kind as CollectionFilter["kind"])) notFound();

  return <CollectionView filter={{ kind, value } as CollectionFilter} />;
}
