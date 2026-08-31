import { File, Paths } from 'expo-file-system';

const CONFIG_FILE = new File(Paths.document, 'photobackup-config.json');

/**
 * https, because photod serves the upload path on TLS only. The scheme is not a
 * preference: a plain-http address reaches the read-only gallery listener at
 * most, and photod refuses every endpoint that carries a token there.
 */
export const DEFAULT_SERVER = 'https://192.168.1.97:8787';

/**
 * 0 means the whole library, which is what a real backup is.
 *
 * Phase 2 defaulted this to 110, the size of the test fixture. That was right
 * while the exit criterion was those 110 items and wrong the moment the app was
 * pointed at a real camera roll, where it would quietly archive the newest
 * hundred photos and call itself up to date. The field is still editable, for
 * deliberately limiting a test run.
 */
export const DEFAULT_MAX_ITEMS = 0;

/**
 * Off, because a backup that starts happening on its own is a thing to be asked
 * for rather than a default to be discovered. See src/background — iOS decides
 * when a background window happens and the honest place to say so is the switch
 * that turns it on.
 */
export const DEFAULT_BACKGROUND_BACKUP = false;

/**
 * Settings, and nothing secret.
 *
 * The device id used to be generated here, at random, and was whatever the phone
 * claimed to be. Since Phase 5 the server issues it during pairing and it lives
 * in the keychain alongside the token that proves it — see src/pairing.ts. An
 * identity the client picks for itself is a label; one the server issues is an
 * identity, and the difference is the whole point of the phase.
 */
export type Config = {
  /** Typed in by hand; the fallback when mDNS finds nothing. */
  serverUrl: string;
  /** The last address that actually answered, tried before the manual one. */
  lastServerUrl: string | null;
  maxItems: number;
  /** Whether iOS may wake the app to advance the queue. See src/background. */
  backgroundBackup: boolean;
};

function defaults(): Config {
  return {
    serverUrl: DEFAULT_SERVER,
    lastServerUrl: null,
    maxItems: DEFAULT_MAX_ITEMS,
    backgroundBackup: DEFAULT_BACKGROUND_BACKUP,
  };
}

export function loadConfig(): Config {
  const fallback = defaults();
  try {
    if (!CONFIG_FILE.exists) {
      saveConfig(fallback);
      return fallback;
    }
    const parsed = JSON.parse(CONFIG_FILE.textSync()) as Partial<Config>;
    return {
      // An http address left over from before Phase 5 is upgraded rather than
      // kept: it would reach the read-only listener and fail every upload with
      // a 426 nobody would think to look for.
      serverUrl: httpsOnly(parsed.serverUrl) ?? fallback.serverUrl,
      lastServerUrl: httpsOnly(parsed.lastServerUrl) ?? null,
      maxItems: normalizeMaxItems(parsed.maxItems, fallback.maxItems),
      // A config file written before this field existed reads as off, which is
      // the same answer a fresh install gives.
      backgroundBackup: parsed.backgroundBackup === true,
    };
  } catch {
    return fallback;
  }
}

export function saveConfig(config: Config) {
  try {
    if (!CONFIG_FILE.exists) CONFIG_FILE.create({ overwrite: true });
    CONFIG_FILE.write(JSON.stringify(config));
  } catch {}
}

/**
 * Rewrites a stored http:// address to https://, leaving anything else alone.
 * Only the scheme changes — the port is the same one photod has always listened
 * on, it is simply TLS now.
 */
function httpsOnly(url: string | null | undefined): string | null {
  if (!url) return null;
  return url.replace(/^http:\/\//i, 'https://');
}

function normalizeMaxItems(value: unknown, fallback: number): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return fallback;
  return Math.floor(value);
}
