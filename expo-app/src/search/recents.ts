import { installRecents } from '@photobackup/core';
import { File, Paths } from 'expo-file-system';

const RECENTS_FILE = new File(Paths.document, 'photobackup-searches.json');

/**
 * The last few things somebody searched for, on this phone.
 *
 * The browser keeps this in `localStorage`, which is what core's
 * `recentSearches` reads by default. There is none here — the same gap
 * `grid/zoomStore.ts` fills for the zoom — so the phone brings the same
 * document-directory JSON the server address, the zoom and the stats cache
 * already use.
 *
 * It is never sent to the server, and that is deliberate rather than
 * incidental: a list of what somebody went looking for is a fact about them
 * rather than about the archive, and it should not outlive them clearing it.
 */
export function installSearchRecents(): void {
  installRecents({
    read(): string[] {
      try {
        if (!RECENTS_FILE.exists) return [];
        const held: unknown = JSON.parse(RECENTS_FILE.textSync());
        return Array.isArray(held) ? (held as unknown[]).filter((e) => typeof e === 'string') : [];
      } catch {
        // Something else's file at this name, or a half-written one. Neither is
        // worth breaking a search box over.
        return [];
      }
    },
    write(list: string[]): void {
      try {
        if (list.length === 0) {
          if (RECENTS_FILE.exists) RECENTS_FILE.delete();
          return;
        }
        RECENTS_FILE.write(JSON.stringify(list));
      } catch {
        // A convenience that did not get written down is not worth a notice.
      }
    },
  });
}
