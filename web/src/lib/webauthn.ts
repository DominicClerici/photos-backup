/**
 * The translation between what WebAuthn speaks and what the wire speaks.
 *
 * The browser's credential API deals in ArrayBuffers; photod's relying-party
 * library encodes every one of them as base64url. These four functions are the
 * whole of the difference, which is why this app needs no WebAuthn library.
 *
 * The sign-in ceremony itself lives in photod, not here — see
 * server/internal/api/signin.html. This module exists for the one ceremony that
 * happens inside the gallery: registering an additional passkey from a browser
 * that is already signed in.
 */

export function base64urlToBytes(value: string): Uint8Array {
  const padded = value.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

export function bytesToBase64url(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let raw = "";
  for (let i = 0; i < bytes.length; i++) raw += String.fromCharCode(bytes[i]);
  return btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** The shape photod sends: the spec's options with every buffer base64url'd. */
interface WireDescriptor {
  id: string;
  type: PublicKeyCredentialType;
  transports?: AuthenticatorTransport[];
}

export interface WireCreationOptions {
  publicKey: Omit<
    PublicKeyCredentialCreationOptions,
    "challenge" | "user" | "excludeCredentials"
  > & {
    challenge: string;
    user: Omit<PublicKeyCredentialUserEntity, "id"> & { id: string };
    excludeCredentials?: WireDescriptor[];
  };
}

/**
 * Turns photod's options into the ones `navigator.credentials.create` wants.
 *
 * Mutating a decoded copy rather than rebuilding the object, so that any field
 * photod's library adds in a future version is passed through untouched instead
 * of being silently dropped by a whitelist written today.
 */
export function decodeCreationOptions(
  wire: WireCreationOptions,
): PublicKeyCredentialCreationOptions {
  const { publicKey } = wire;
  return {
    ...publicKey,
    challenge: base64urlToBytes(publicKey.challenge),
    user: { ...publicKey.user, id: base64urlToBytes(publicKey.user.id) },
    excludeCredentials: (publicKey.excludeCredentials ?? []).map((d) => ({
      ...d,
      id: base64urlToBytes(d.id),
    })),
  } as PublicKeyCredentialCreationOptions;
}

/** What photod expects back from a registration. */
export interface WireAttestation {
  id: string;
  rawId: string;
  type: string;
  response: { clientDataJSON: string; attestationObject: string };
}

export function encodeAttestation(credential: PublicKeyCredential): WireAttestation {
  const response = credential.response as AuthenticatorAttestationResponse;
  return {
    id: credential.id,
    rawId: bytesToBase64url(credential.rawId),
    type: credential.type,
    response: {
      clientDataJSON: bytesToBase64url(response.clientDataJSON),
      attestationObject: bytesToBase64url(response.attestationObject),
    },
  };
}

/**
 * Turns a failed ceremony into something worth showing.
 *
 * A cancelled prompt is not an error worth alarming anybody about — somebody
 * pressed escape — and `InvalidStateError` has one specific meaning that the
 * browser's own wording does not convey: this authenticator already holds a
 * passkey for this archive, which is what the exclusion list is for.
 */
export function explainCeremonyError(err: unknown): string {
  if (err instanceof DOMException) {
    if (err.name === "NotAllowedError" || err.name === "AbortError") return "Cancelled.";
    if (err.name === "InvalidStateError") {
      return "This device already has a passkey for this archive.";
    }
  }
  if (err instanceof Error && err.message) return err.message;
  return "Something went wrong.";
}

/** Whether this browser can do any of the above. */
export function supportsPasskeys(): boolean {
  return typeof window !== "undefined" && Boolean(window.PublicKeyCredential);
}
