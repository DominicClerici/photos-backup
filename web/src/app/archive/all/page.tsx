import { VaultTimelineView } from "@/components/VaultTimelineView";

// Everything in the bucket as one timeline. The library has no equivalent
// because it does not need one — the Gallery tab *is* that page — but a bucket's
// front page is its collections, so this is the way to the photographs
// themselves.
export default function Page() {
  return <VaultTimelineView bucket="archive" />;
}
