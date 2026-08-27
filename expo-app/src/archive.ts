import { configure, installNotifier, rearmUnauthorized } from '@photobackup/core';

/**
 * Where the archive is, from this phone, and what proves it is this phone.
 *
 * Both are module state rather than values captured anywhere, and both are read
 * per request. That is the same shape GalleryClient and HttpTransport already
 * had, and for the same reason their comments give: discovery moving from the
 * LAN to Tailscale and a re-pairing both have to take effect without whatever
 * is mid-flight being rebuilt. What is new is that they now feed
 * `@photobackup/core` as well, so the shared client and the sync engine agree
 * about where they are pointing by construction rather than by being handed the
 * same two closures.
 *
 * The global is honest: there is one archive.
 */
let address = '';
let token: string | null = null;
let onLost: () => void = () => {};

/** Trailing slashes stripped, because every path in core begins with one. */
export function archiveAddress(): string {
  return address;
}

export function setArchiveAddress(url: string): void {
  address = url.replace(/\/+$/, '');
}

export function archiveToken(): string | null {
  return token;
}

/**
 * A new credential, or none.
 *
 * Storing one re-arms the dead-credential notice: core latches it so that a
 * grid's worth of simultaneous 401s is one trip to the pairing screen rather
 * than two hundred, and on a phone — which stays running and comes back from
 * that screen — the latch has to be released again on the way out.
 */
export function setArchiveToken(next: string | null): void {
  token = next;
  if (next) rearmUnauthorized();
}

/**
 * What to do when photod says this phone's token is no longer good.
 *
 * Registered by the app rather than decided here: in Phase 1 it drops the
 * credential and lets the one screen notice; once there is a router it will be
 * a route to pairing. Either way the token is cleared first, so nothing else
 * sends the dead one again.
 */
export function onCredentialLost(handler: () => void): void {
  onLost = handler;
}

configure({
  baseUrl: archiveAddress,

  /**
   * A missing token is a missing header rather than an empty one, so photod
   * answers "a device token is required" instead of rejecting a malformed
   * credential — two different messages on a phone screen.
   *
   * This is also what `media()` hands back beside every rendition URL, and the
   * whole reason the in-app gallery is a different problem from the browser's:
   * expo-image, expo-video and File.downloadFileAsync all take headers, so
   * every rendition authenticates with the token already in the keychain and no
   * second credential has to exist.
   */
  headers: (): Record<string, string> =>
    token ? { authorization: `Bearer ${token}` } : {},

  onUnauthorized: () => {
    token = null;
    onLost();
  },
});

/**
 * Somewhere for core's hooks to report a failed write.
 *
 * The console until Phase 2 gives the app a Toast to put them in. A no-op would
 * have done — core's default is one — but a delete that fails silently during
 * the port is exactly the thing worth being able to see in the Metro log.
 */
installNotifier({
  add: (notice) => {
    const body = [notice.title, notice.description].filter(Boolean).join(' — ');
    if (notice.type === 'error') console.warn(`[archive] ${body}`);
    else console.log(`[archive] ${body}`);
    return '';
  },
  close: () => {},
});
