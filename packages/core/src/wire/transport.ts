/**
 * The one thing the two clients disagree about.
 *
 * Everything else in `api.ts` — the wire types, the paths, the query strings,
 * the error vocabulary — is the same on a phone as in a browser. What differs
 * is three facts: where the archive is, what proves who is asking, and what to
 * do when the answer is that nobody is. So those three are an interface, and
 * the rest of the file is written once.
 *
 * A browser reaches photod at `/api` under a same-origin session cookie the
 * fetch attaches by itself; a phone reaches it at whatever address discovery
 * settled on, carrying the device token from the keychain. Neither knows about
 * the other.
 */
export interface Transport {
  /**
   * Read per request, never captured.
   *
   * Discovery moves an address from the LAN to Tailscale between one fetch and
   * the next, and nothing holding a reference should have to be rebuilt for
   * that. Web returns a constant here and pays nothing for the call.
   */
  baseUrl(): string;

  /**
   * Likewise per request: a re-pairing must take effect without anything being
   * reconstructed. Returns `{}` in the browser, where the cookie rides along
   * without being asked for.
   */
  headers(): Record<string, string>;

  /**
   * What a 401 means here.
   *
   * The browser has a session that ended and a sign-in page photod serves
   * itself; the phone has a device token that has been revoked and a pairing
   * screen. Called at most once per outage — `api.ts` guards it — because a
   * grid firing two hundred thumbnail requests sees two hundred 401s inside a
   * few milliseconds and must not act on each.
   */
  onUnauthorized(): void;
}

/**
 * A transport that answers nothing, so that an unconfigured import fails with
 * a sentence rather than with `undefined is not a function` four frames down.
 *
 * There is one archive, so this is a module-level value rather than something
 * threaded through every hook and every component. The global is honest.
 */
let current: Transport = {
  baseUrl(): string {
    throw new Error("@photobackup/core: configure(transport) has not been called");
  },
  headers: () => ({}),
  onUnauthorized: () => {},
};

/** Installed once at startup, before anything reads. */
export function configure(transport: Transport): void {
  current = transport;
  latched = false;
}

export function baseUrl(): string {
  return current.baseUrl();
}

export function headers(): Record<string, string> {
  return current.headers();
}

/**
 * Says the credential is dead, at most once — see api.ts, where a grid firing
 * two hundred thumbnail requests sees two hundred 401s in a few milliseconds.
 */
let latched = false;

export function unauthorized(): void {
  if (latched) return;
  latched = true;
  current.onUnauthorized();
}

/**
 * Arms it again.
 *
 * The browser never needs this: the one thing it does about a dead session is
 * leave, and a page that is leaving has no use for a second notice. A phone
 * does — it stays running, the pairing screen is somewhere it comes back from,
 * and a token revoked a second time has to be noticed a second time. Called
 * when a new credential is stored, and by `configure` because installing a
 * transport is the start of something.
 */
export function rearmUnauthorized(): void {
  latched = false;
}
