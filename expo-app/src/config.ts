import { File, Paths } from 'expo-file-system';

const CONFIG_FILE = new File(Paths.document, 'photobackup-config.json');

export const DEFAULT_SERVER = 'http://10.0.4.120:8787';

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

export type Config = {
  /** Typed in by hand; the fallback when mDNS finds nothing. */
  serverUrl: string;
  /** The last address that actually answered, tried before the manual one. */
  lastServerUrl: string | null;
  deviceId: string;
  maxItems: number;
};

/**
 * The device id is generated once and kept, so re-uploads from this phone stay
 * attributable after a reinstall of the JS bundle. It is not an identity or a
 * credential — pairing arrives in a later phase.
 */
function generateDeviceId(): string {
  return `ios-${Math.random().toString(36).slice(2, 10)}`;
}

function defaults(): Config {
  return {
    serverUrl: DEFAULT_SERVER,
    lastServerUrl: null,
    deviceId: generateDeviceId(),
    maxItems: DEFAULT_MAX_ITEMS,
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
    const config: Config = {
      serverUrl: parsed.serverUrl ?? fallback.serverUrl,
      lastServerUrl: parsed.lastServerUrl ?? null,
      deviceId: parsed.deviceId ?? fallback.deviceId,
      maxItems: normalizeMaxItems(parsed.maxItems, fallback.maxItems),
    };
    if (!parsed.deviceId) saveConfig(config);
    return config;
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

function normalizeMaxItems(value: unknown, fallback: number): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return fallback;
  return Math.floor(value);
}
