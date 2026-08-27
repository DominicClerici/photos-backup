import { File, Paths } from 'expo-file-system';

import { DEFAULT_ZOOM, MAX_ZOOM } from '@photobackup/core';

import { clamp } from './geometry';

const ZOOM_FILE = new File(Paths.document, 'photobackup-zoom.json');

/**
 * How big the tiles were last time.
 *
 * The browser keeps this in `localStorage`, which is what `lib/zoom.savedZoom`
 * reads. There is no `localStorage` here — the optional chaining in that
 * function means it quietly answers the default forever — so the phone keeps
 * its own, in the same document-directory JSON the server address and the stats
 * cache use. One integer, and losing it costs a pinch.
 */
export function savedLevel(): number {
  try {
    if (!ZOOM_FILE.exists) return DEFAULT_ZOOM;
    const parsed = JSON.parse(ZOOM_FILE.textSync()) as { level?: unknown };
    return typeof parsed.level === 'number' && Number.isInteger(parsed.level)
      ? clamp(parsed.level, 0, MAX_ZOOM)
      : DEFAULT_ZOOM;
  } catch {
    return DEFAULT_ZOOM;
  }
}

export function rememberLevel(level: number): void {
  if (!Number.isInteger(level)) return;
  try {
    ZOOM_FILE.write(JSON.stringify({ level }));
  } catch {
    // A zoom is not worth failing a render over.
  }
}
