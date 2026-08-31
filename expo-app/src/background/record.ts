import { File, Paths } from 'expo-file-system';

/**
 * What the last background window did, so the settings card can say.
 *
 * A background run has no screen to report to. It happens while the phone is on
 * a bedside table, and the only evidence it ever ran is whatever it wrote down
 * before iOS suspended the app again — so this file is not a nicety, it is the
 * whole feedback loop. Without it "background backup is on" is a claim nobody
 * can check, which is exactly what PROJECT.md § 8 risk 2 says the UI must not
 * make.
 *
 * Stored beside the config and the album selection, and for the same reasons:
 * small, not secret, and meaningless on another phone. Losing it loses one
 * sentence of history, which is why nothing here throws.
 */

const RECORD_FILE = new File(Paths.document, 'photobackup-background.json');

export type BackgroundResult =
  /** The engine ran and moved the queue along. */
  | 'worked'
  /** The conditions gate said not now — not charging, or not on Wi-Fi. */
  | 'held'
  /** There was nothing to do, or nothing that could be done. */
  | 'skipped'
  /** Something went wrong that is worth reading. */
  | 'failed';

export type BackgroundOutcome = {
  /** Device clock when the window ended. */
  at: number;
  result: BackgroundResult;
  /** One line, written to be read in settings rather than parsed. */
  detail: string;
  /** How long the window lasted, in milliseconds. */
  durationMs: number;
  /** Items still owing when the window ended, or null if it never got that far. */
  remaining: number | null;
  /** True when iOS cut the window short rather than the queue running out. */
  expired: boolean;
};

export function loadOutcome(): BackgroundOutcome | null {
  try {
    if (!RECORD_FILE.exists) return null;
    const parsed = JSON.parse(RECORD_FILE.textSync()) as Partial<BackgroundOutcome>;
    if (typeof parsed?.at !== 'number' || !Number.isFinite(parsed.at)) return null;
    if (typeof parsed.detail !== 'string') return null;
    if (!isResult(parsed.result)) return null;
    return {
      at: parsed.at,
      result: parsed.result,
      detail: parsed.detail,
      durationMs: typeof parsed.durationMs === 'number' ? parsed.durationMs : 0,
      remaining: typeof parsed.remaining === 'number' ? parsed.remaining : null,
      expired: parsed.expired === true,
    };
  } catch {
    return null;
  }
}

export function saveOutcome(outcome: BackgroundOutcome) {
  try {
    if (!RECORD_FILE.exists) RECORD_FILE.create({ overwrite: true });
    RECORD_FILE.write(JSON.stringify(outcome));
  } catch {}
}

/**
 * Forgets what the last window did. Called when background backup is switched
 * off, so turning it back on months later does not open on a stale sentence
 * about a run from before.
 */
export function clearOutcome() {
  try {
    if (RECORD_FILE.exists) RECORD_FILE.delete();
  } catch {}
}

function isResult(value: unknown): value is BackgroundResult {
  return value === 'worked' || value === 'held' || value === 'skipped' || value === 'failed';
}
