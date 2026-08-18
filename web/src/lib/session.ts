/**
 * The browser's half of the gallery password.
 *
 * Everything the sign-in feature adds to the web app is this file,
 * `@/hooks/useSession`, `@/components/SignIn`, one line in the root layout, and
 * one line in `api.ts` — all four marked "browser gate". Deleting them, and
 * `internal/websession` on the server, removes the feature completely; nothing
 * else imports any of it.
 *
 * It deliberately does not live in `api.ts` beside the rest of the endpoints,
 * even though it is three more fetches. `api.ts` is the archive's API — the
 * timeline, the assets, the vault — and this is the door, which is a thing that
 * may well be replaced wholesale by a real identity provider later. Keeping it
 * separate is what makes that a deletion rather than an edit.
 */

// The same base `api.ts` uses, repeated rather than imported so this module
// depends on nothing. In development the Next rewrite sends it to photod's
// loopback listener, which answers "no session required"; in production photod
// serves the app itself and answers for real.
const BASE = "/api";

export interface SessionStatus {
  /** Whether this server has a gallery password at all. */
  required: boolean;
  signedIn: boolean;
  /** When the session lapses if the gallery is left alone. */
  expiresAt: Date | null;
}

interface RawStatus {
  required: boolean;
  signed_in: boolean;
  expires_at?: string;
}

/**
 * Broadcast when a request comes back 401, so that a session which lapsed
 * mid-scroll puts the password prompt up rather than filling the screen with
 * failures.
 *
 * A module-level subscription for the same reason the vault's prompt is one:
 * the thing that discovers the session is gone is a fetch three components
 * deep, and the thing that has to react is mounted by the root layout.
 */
type Listener = () => void;
let listeners: Listener[] = [];

/** Called by `api.ts` on any 401. The only thing outside this feature that
 * knows it exists. */
export function signedOut() {
  for (const listener of listeners) listener();
}

export function onSignedOut(listener: Listener): () => void {
  listeners = [...listeners, listener];
  return () => {
    listeners = listeners.filter((l) => l !== listener);
  };
}

function parse(raw: RawStatus): SessionStatus {
  return {
    required: raw.required,
    signedIn: raw.signed_in,
    expiresAt: raw.expires_at ? new Date(raw.expires_at) : null,
  };
}

/**
 * Asks whether this browser is signed in.
 *
 * Throws only when photod cannot be reached at all. A server that has no
 * gallery password answers `required: false`, which is how the development
 * gallery — talking to the loopback listener that needs no session — never
 * draws a prompt.
 */
export async function fetchSession(signal?: AbortSignal): Promise<SessionStatus> {
  const res = await fetch(`${BASE}/v1/session`, { signal });
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return parse((await res.json()) as RawStatus);
}

export async function signIn(password: string): Promise<SessionStatus> {
  const res = await fetch(`${BASE}/v1/session`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  if (!res.ok) throw new Error(await errorText(res));
  return parse((await res.json()) as RawStatus);
}

export async function signOut(): Promise<void> {
  const res = await fetch(`${BASE}/v1/session`, { method: "DELETE" });
  if (!res.ok) throw new Error(await errorText(res));
}

async function errorText(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // Not photod answering — a proxy, or nothing at all.
  }
  return `${res.status} ${res.statusText}`;
}
