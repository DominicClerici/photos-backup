import { TagCleanup } from "@/components/TagCleanup";

// Its own route rather than a panel on /status, for the reason /merge has one: a
// card on the status page is a summary of the archive, and this is a place where
// the archive is changed. Everything here rewrites what every photograph is
// findable by.
export default function Page() {
  return <TagCleanup />;
}
