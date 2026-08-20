import { Suspense } from "react";

import { SearchView } from "@/components/SearchView";

/**
 * The whole ranking, for a question the palette has already asked.
 *
 * A Suspense boundary because the page reads the query out of the URL, and
 * `useSearchParams` opts whatever reads it into client rendering. The fallback
 * is deliberately not a spinner: the header is the same shape either way, and a
 * spinner that flashes for one frame on every navigation is worse than nothing.
 */
export default function Page() {
  return (
    <Suspense fallback={<div className="h-dvh" />}>
      <SearchView />
    </Suspense>
  );
}
