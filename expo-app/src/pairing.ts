import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';

import { errorText, SyncError } from './sync/types';

const CREDENTIAL_KEY = 'photobackup.credential';

/**
 * The device token, and the server-issued identity that came with it.
 *
 * Kept in the iOS keychain rather than beside the rest of the config, which is a
 * plain JSON file in the app's documents directory. A bearer token that never
 * expires is the one thing in this app worth protecting properly.
 */
export type Credential = {
  deviceId: string;
  token: string;
  /** The server's hostname, so the screen can name what it is paired to. */
  serverName: string;
  /** Which address the pairing was done against, for the same reason. */
  serverUrl: string;
  pairedAt: number;
};

/**
 * AFTER_FIRST_UNLOCK, not the WHEN_UNLOCKED default.
 *
 * A background top-up runs whenever iOS decides to run it, which is frequently
 * while the phone is locked in a pocket. With the default the keychain read would
 * fail there and the upload would look like an auth failure rather than a locked
 * device — the token would appear to have stopped working, on exactly the runs
 * nobody is watching.
 */
const KEYCHAIN_OPTIONS = { keychainAccessible: SecureStore.AFTER_FIRST_UNLOCK };

export async function loadCredential(): Promise<Credential | null> {
  try {
    const raw = await SecureStore.getItemAsync(CREDENTIAL_KEY, KEYCHAIN_OPTIONS);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<Credential>;
    if (!parsed.deviceId || !parsed.token) return null;
    return {
      deviceId: parsed.deviceId,
      token: parsed.token,
      serverName: parsed.serverName ?? 'the server',
      serverUrl: parsed.serverUrl ?? '',
      pairedAt: parsed.pairedAt ?? 0,
    };
  } catch {
    // An unreadable keychain is indistinguishable from an unpaired device as far
    // as anything downstream is concerned, and the recovery is the same: pair.
    return null;
  }
}

export async function saveCredential(credential: Credential): Promise<void> {
  await SecureStore.setItemAsync(CREDENTIAL_KEY, JSON.stringify(credential), KEYCHAIN_OPTIONS);
}

export async function clearCredential(): Promise<void> {
  try {
    await SecureStore.deleteItemAsync(CREDENTIAL_KEY, KEYCHAIN_OPTIONS);
  } catch {
    // Nothing to do about it, and the token it failed to delete is one the
    // server has already stopped honouring.
  }
}

/**
 * Redeems a pairing code.
 *
 * The code is a shared secret with a ten-minute life, and this is the only
 * request in the app that does not carry a token — it is where the token comes
 * from. Server trust is not established here: the CA is installed out of band
 * before this can succeed at all, which is why a failure to connect and a
 * failure to trust look the same from here and the message says so.
 */
export async function pair(options: {
  serverUrl: string;
  code: string;
  deviceName: string;
}): Promise<Credential> {
  const { serverUrl, code, deviceName } = options;
  const base = serverUrl.replace(/\/+$/, '');

  let response: Response;
  try {
    response = await fetch(`${base}/v1/pair`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        code,
        name: deviceName.trim() || 'iPhone',
        platform: Platform.OS,
      }),
    });
  } catch (e) {
    // React Native reports a rejected certificate and an unreachable host with
    // the same "Network request failed", so this cannot tell them apart and must
    // not pretend to.
    throw new SyncError(
      `could not reach ${base}: ${errorText(e)}. If photod is running, its CA is probably not installed and trusted on this phone yet — run \`photobackup ca --serve\` on the server.`,
      'unreachable'
    );
  }

  if (!response.ok) {
    throw new SyncError(await pairingProblem(response), 'item', response.status);
  }

  let parsed: { deviceId?: string; token?: string; serverName?: string };
  try {
    parsed = (await response.json()) as typeof parsed;
  } catch (e) {
    throw new SyncError(`unreadable pairing response: ${errorText(e)}`, 'item');
  }
  if (!parsed.deviceId || !parsed.token) {
    throw new SyncError('the server paired without returning a token', 'item');
  }

  const credential: Credential = {
    deviceId: parsed.deviceId,
    token: parsed.token,
    serverName: parsed.serverName || 'the server',
    serverUrl: base,
    pairedAt: Date.now(),
  };
  await saveCredential(credential);
  return credential;
}

/** Turns a rejected pairing into something worth reading on a phone screen. */
async function pairingProblem(response: Response): Promise<string> {
  const detail = await serverMessage(response);
  switch (response.status) {
    case 400:
      return detail || 'that does not look like a pairing code — it is eight characters';
    case 403:
      return detail || 'that code is not valid; it may have expired or already been used';
    case 429:
      return 'too many attempts; wait a few minutes and try again';
    default:
      return detail || `pairing failed with status ${response.status}`;
  }
}

async function serverMessage(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: string };
    return body?.error ?? '';
  } catch {
    return '';
  }
}
