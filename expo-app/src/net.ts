import { File, Paths } from 'expo-file-system';

import { log } from './log';

const CONFIG_FILE = new File(Paths.document, 'phase0-config.json');
export const DEFAULT_SERVER = 'http://10.0.4.120:8787';

export function loadServerUrl(): string {
  try {
    if (!CONFIG_FILE.exists) return DEFAULT_SERVER;
    return JSON.parse(CONFIG_FILE.textSync()).serverUrl ?? DEFAULT_SERVER;
  } catch {
    return DEFAULT_SERVER;
  }
}

export function saveServerUrl(serverUrl: string) {
  try {
    if (!CONFIG_FILE.exists) CONFIG_FILE.create({ overwrite: true });
    CONFIG_FILE.write(JSON.stringify({ serverUrl }));
  } catch {}
}

export type TimelineEvent = {
  seq: number;
  at: string;
  ms: number;
  event: string;
  detail: Record<string, unknown>;
};

export async function ping(serverUrl: string) {
  const t0 = Date.now();
  const res = await fetch(`${serverUrl}/health`);
  const body = await res.json();
  return { ms: Date.now() - t0, body };
}

/** Pushes a client observation into the server's timeline so both share a clock. */
export async function note(serverUrl: string, detail: Record<string, unknown>) {
  try {
    await fetch(`${serverUrl}/note`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ ...detail, clientTime: new Date().toISOString() }),
    });
  } catch (e) {
    log('net', `note failed: ${String(e)}`);
  }
}

export async function fetchTimeline(serverUrl: string, since = 0): Promise<TimelineEvent[]> {
  const res = await fetch(`${serverUrl}/timeline?since=${since}`);
  const body = await res.json();
  return body.events ?? [];
}

export async function resetTimeline(serverUrl: string) {
  await fetch(`${serverUrl}/timeline`, { method: 'DELETE' });
}
