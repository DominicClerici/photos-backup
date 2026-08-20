import { MergeReview } from "@/components/MergeReview";

// Its own route rather than /overview/duplicates, and for the reason the trash
// has one: a card on the overview is a summary of the archive, and this is a
// place where the archive is changed. The page covers both kinds of merge —
// the duplicates somebody is asked about and the split recordings the worker
// joined without asking — because they came out of the same scan.
export default function Page() {
  return <MergeReview />;
}
