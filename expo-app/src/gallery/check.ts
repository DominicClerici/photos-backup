import { errorText, isUnauthorized, SyncError } from '../sync/types';
import type { GalleryClient } from './client';

/**
 * One line of the access check: what was tried, and what came back.
 *
 * `ok` is whether the expectation held, which is not the same as whether the
 * request succeeded — the anonymous step passes by being refused.
 */
export type CheckStep = {
  label: string;
  ok: boolean;
  detail: string;
};

export type CheckResult = {
  steps: CheckStep[];
  /** True when every step held. */
  ok: boolean;
  /** Set when a step failed because this phone's pairing is no longer good. */
  unauthorized: boolean;
};

/**
 * Proves the phone can read the archive, and that nothing else can.
 *
 * This is scaffolding for the gallery rather than the gallery: it exercises the
 * three things the port will depend on and reports them in a form a screen can
 * render.
 *
 *  1. JSON reads authenticate — the timeline comes back with the token.
 *  2. Media reads authenticate — a thumbnail comes back with the same header,
 *     which is the step a browser cannot take and the reason the in-app gallery
 *     needs no signed URLs.
 *  3. The same request without the token is refused — so step 1 passing means
 *     the pairing is doing the work, not that the door is open.
 *
 * Step 3 is the one worth keeping once the real gallery lands. A read path that
 * quietly stopped checking tokens would leave the other two green.
 */
export async function checkGalleryAccess(client: GalleryClient): Promise<CheckResult> {
  const steps: CheckStep[] = [];
  let unauthorized = false;

  let firstAssetId: string | null = null;
  try {
    const page = await client.timeline({ limit: 1 });
    firstAssetId = page.items[0]?.id ?? null;
    steps.push({
      label: 'Read the timeline',
      ok: true,
      detail: firstAssetId
        ? `newest asset ${firstAssetId.slice(0, 8)}, taken ${page.items[0].taken_at.slice(0, 10)}`
        : 'the archive is empty, but the read was accepted',
    });
  } catch (e) {
    unauthorized = unauthorized || isUnauthorized(e);
    steps.push({ label: 'Read the timeline', ok: false, detail: errorText(e) });
  }

  if (firstAssetId) {
    try {
      const { bytes, contentType } = await client.media(firstAssetId, 'thumb');
      steps.push({
        label: 'Fetch a thumbnail',
        ok: true,
        detail: `${contentType}, ${formatBytes(bytes)}`,
      });
    } catch (e) {
      unauthorized = unauthorized || isUnauthorized(e);
      // A derivative that has not been generated yet is a 404, and it is not an
      // access failure — the request was authorized, there is simply nothing
      // there. Saying so beats reporting a security problem that is not one.
      const pending = e instanceof SyncError && e.status === 404;
      steps.push({
        label: 'Fetch a thumbnail',
        ok: pending,
        detail: pending ? 'authorized, but this thumbnail is not generated yet' : errorText(e),
      });
    }
  } else {
    steps.push({
      label: 'Fetch a thumbnail',
      ok: true,
      detail: 'skipped — nothing in the archive to fetch',
    });
  }

  steps.push(await checkAnonymousReadIsRefused(client));

  return { steps, ok: steps.every((step) => step.ok), unauthorized };
}

/**
 * Asks for the timeline with no credential at all and expects to be turned away.
 *
 * Built by hand rather than through the client, because the client's whole job
 * is to attach the token and there should be no way to ask it not to. The
 * request is aimed at the same base URL the client uses, so this tests the
 * listener the phone actually talks to.
 */
async function checkAnonymousReadIsRefused(client: GalleryClient): Promise<CheckStep> {
  const label = 'Refuse the same read without the token';
  const url = client.url('/v1/timeline?limit=1');

  let response: Response;
  try {
    response = await fetch(url, { method: 'GET' });
  } catch (e) {
    return { label, ok: false, detail: `could not reach the server: ${errorText(e)}` };
  }

  if (response.status === 401) {
    return { label, ok: true, detail: '401, as it should be' };
  }
  return {
    label,
    ok: false,
    detail:
      response.status === 200
        ? 'the archive answered an unpaired caller — the read path is open'
        : `expected 401, got ${response.status}`,
  };
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
