/**
 * What the archive is told about a shared photograph, assembled from the two
 * places that each know half of it.
 *
 * Pure, and importing nothing but types, for the reason summary.ts is: media.ts
 * reaches into PhotoKit and expo-file-system and cannot be loaded under jest,
 * and this is the part of it worth checking.
 */

import type { PhotoKitFacts } from '../../modules/photo-facts';
import type { SharedOrigin } from '../sync/types';

/**
 * PhotoKit's own answer, with what only the enumeration knew put back into it.
 *
 * The native module is asked again at describe time because most of what it
 * reports — the pixel dimensions, the resource list, whether the shot was ever
 * edited — is about this asset now. Two fields are not answerable that way at
 * all, and those are the two this restores. See SharedOrigin.
 *
 * A live read still wins where there is one. If some future iOS does start
 * naming a photograph's shared albums when asked, this stops overriding it the
 * day that happens rather than the day somebody notices.
 */
export function sharedFacts(
  facts: PhotoKitFacts | null,
  origin: SharedOrigin | null
): PhotoKitFacts | null {
  if (facts === null || origin === null) return facts;
  return {
    ...facts,
    sharedAlbums: albumTitles(facts.sharedAlbums, origin.albums),
    contributor: facts.contributor ?? origin.contributor,
  };
}

/**
 * The albums an asset is in, from both places that know, once each.
 *
 * Deduplicated because a title is the whole identity of an album on the server —
 * sending "Iceland" twice would be one album either way, and asking twice is
 * still two round trips.
 */
export function albumTitles(library: string[], shared: string[]): string[] {
  const titles = new Set<string>();
  for (const title of [...library, ...shared]) {
    const trimmed = title.trim();
    if (trimmed !== '') titles.add(trimmed);
  }
  return [...titles];
}
