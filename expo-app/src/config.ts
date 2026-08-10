import { File, Paths } from 'expo-file-system';

const CONFIG_FILE = new File(Paths.document, 'photobackup-config.json');

export const DEFAULT_SERVER = 'http://10.0.4.120:8787';

type Config = { serverUrl: string; deviceId: string };

/**
 * The device id is generated once and kept, so re-uploads from this phone stay
 * attributable after a reinstall of the JS bundle. It is not an identity or a
 * credential — pairing arrives in a later phase.
 */
function generateDeviceId(): string {
  return `ios-${Math.random().toString(36).slice(2, 10)}`;
}

export function loadConfig(): Config {
  const fallback = { serverUrl: DEFAULT_SERVER, deviceId: generateDeviceId() };
  try {
    if (!CONFIG_FILE.exists) {
      saveConfig(fallback);
      return fallback;
    }
    const parsed = JSON.parse(CONFIG_FILE.textSync()) as Partial<Config>;
    const config = {
      serverUrl: parsed.serverUrl ?? fallback.serverUrl,
      deviceId: parsed.deviceId ?? fallback.deviceId,
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
