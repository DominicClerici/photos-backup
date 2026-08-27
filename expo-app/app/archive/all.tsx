import { BucketTimeline } from '../../src/vault';

/**
 * Everything in the bucket as one timeline. The library has no equivalent
 * because it does not need one — the Gallery tab *is* that screen — but a
 * bucket's front page is its collections, so this is the way to the photographs
 * themselves.
 */
export default function ArchiveAllRoute() {
  return <BucketTimeline bucket="archive" />;
}
