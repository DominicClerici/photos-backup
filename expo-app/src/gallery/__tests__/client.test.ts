import { isUnauthorized, SyncError } from '../../sync/types';
import { checkGalleryAccess } from '../check';
import { GalleryClient } from '../client';

/** One recorded call, so a test can assert what went out as well as what came back. */
type Call = { url: string; init: RequestInit | undefined };

type Route = [fragment: string, answer: (call: Call) => Response];

/**
 * A fetch that answers from a routing table keyed on a substring of the URL.
 *
 * Substring rather than exact match because the timeline carries a query string
 * that is not what any of these tests are about. Each answer is handed the call,
 * so a route can behave like photod does and decide on the Authorization header.
 */
function fakeFetch(routes: Route[]) {
  const calls: Call[] = [];
  const impl = jest.fn(async (url: string, init?: RequestInit) => {
    const call = { url, init };
    calls.push(call);
    const route = routes.find(([fragment]) => url.includes(fragment));
    if (!route) throw new Error(`no fake route for ${url}`);
    return route[1](call);
  });
  globalThis.fetch = impl as unknown as typeof fetch;
  return { impl, calls };
}

function authorization(call: Call): string | undefined {
  return (call.init?.headers as Record<string, string> | undefined)?.authorization;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function bytes(length: number, contentType: string): Response {
  return new Response(new Uint8Array(length), {
    status: 200,
    headers: { 'content-type': contentType, 'content-length': String(length) },
  });
}

function clientWith(token: string | null, base = 'https://server.local:8787') {
  return new GalleryClient(
    () => base,
    () => token
  );
}

const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});

/** The header on every request is what the whole design rests on. */
test('sends the device token on json and media reads alike', async () => {
  const fetcher = fakeFetch([
    ['/v1/timeline', () => json({ items: [] })],
    ['/thumb', () => bytes(4096, 'image/webp')],
  ]);

  const client = clientWith('pbk_secret');
  await client.timeline();
  await client.media('asset-1', 'thumb');

  expect(fetcher.calls).toHaveLength(2);
  for (const call of fetcher.calls) {
    expect(authorization(call)).toBe('Bearer pbk_secret');
  }
});

// An empty header would be rejected as a malformed credential rather than as a
// missing one, and the two are a different message on the phone.
test('omits the header entirely when there is no token', async () => {
  const fetcher = fakeFetch([['/v1/timeline', () => json({ items: [] })]]);

  await clientWith(null).timeline();

  expect(fetcher.calls[0].init?.headers).not.toHaveProperty('authorization');
});

// The address and the token are read per request, so discovery moving the server
// and a re-pairing both land without the client being rebuilt.
test('follows a changing address and a changing token', async () => {
  const fetcher = fakeFetch([['/v1/timeline', () => json({ items: [] })]]);

  let base = 'https://lan.local:8787';
  let token: string | null = 'pbk_first';
  const client = new GalleryClient(
    () => base,
    () => token
  );

  await client.timeline();
  base = 'https://tailscale.local:8787';
  token = 'pbk_second';
  await client.timeline();

  expect(fetcher.calls[0].url).toContain('lan.local');
  expect(fetcher.calls[1].url).toContain('tailscale.local');
  expect(authorization(fetcher.calls[1])).toBe('Bearer pbk_second');
});

// A revoked token has to surface as "pair again" rather than as a broken tile,
// in the same vocabulary the sync engine already acts on.
test('reports a refused read as unauthorized', async () => {
  fakeFetch([['/v1/timeline', () => json({ error: 'this device has been unpaired' }, 401)]]);

  const error = await clientWith('pbk_revoked')
    .timeline()
    .catch((e: unknown) => e);

  expect(isUnauthorized(error)).toBe(true);
  expect((error as SyncError).status).toBe(401);
});

test('trims a trailing slash off the server address', () => {
  expect(clientWith('t', 'https://server.local:8787/').mediaUrl('abc', 'preview')).toBe(
    'https://server.local:8787/v1/assets/abc/preview'
  );
});

describe('the access check', () => {
  const item = {
    id: 'a1b2c3d4-0000-4000-8000-000000000000',
    kind: 'image',
    taken_at: '2026-08-01T12:00:00Z',
    state: 'ready',
  };

  /** photod since Phase 6: reads on the TLS listener want a token. */
  const guardedTimeline: Route = [
    '/v1/timeline',
    (call) => (authorization(call) ? json({ items: [item] }) : json({ error: 'no token' }, 401)),
  ];

  test('passes when reads are authorized and anonymous reads are refused', async () => {
    fakeFetch([['/thumb', () => bytes(12_800, 'image/webp')], guardedTimeline]);

    const result = await checkGalleryAccess(clientWith('pbk_good'));

    expect(result.steps.map((step) => [step.label, step.ok])).toEqual([
      ['Read the timeline', true],
      ['Fetch a thumbnail', true],
      ['Refuse the same read without the token', true],
    ]);
    expect(result.ok).toBe(true);
    expect(result.unauthorized).toBe(false);
  });

  // The step that earns its keep: the other two can be green while the archive
  // is readable by anyone who can reach the port.
  test('fails when the server answers an unpaired caller', async () => {
    fakeFetch([
      ['/thumb', () => bytes(12_800, 'image/webp')],
      ['/v1/timeline', () => json({ items: [item] })],
    ]);

    const result = await checkGalleryAccess(clientWith('pbk_good'));

    expect(result.ok).toBe(false);
    expect(result.steps[2].detail).toContain('the read path is open');
  });

  test('flags a revoked pairing rather than blaming the network', async () => {
    fakeFetch([guardedTimeline]);

    const result = await checkGalleryAccess(clientWith(null));

    expect(result.unauthorized).toBe(true);
    expect(result.ok).toBe(false);
  });

  // A thumbnail the worker has not made yet is a 404 on an authorized request.
  // Reporting it as an access failure would send somebody hunting a security
  // problem that is not there.
  test('does not mistake a missing derivative for a refusal', async () => {
    fakeFetch([
      ['/thumb', () => json({ error: 'derivative not generated yet' }, 404)],
      guardedTimeline,
    ]);

    const result = await checkGalleryAccess(clientWith('pbk_good'));

    expect(result.steps[1].ok).toBe(true);
    expect(result.steps[1].detail).toContain('not generated yet');
  });

  test('reports an empty archive as a pass, not a failure', async () => {
    fakeFetch([
      [
        '/v1/timeline',
        (call) => (authorization(call) ? json({ items: [] }) : json({ error: 'no token' }, 401)),
      ],
    ]);

    const result = await checkGalleryAccess(clientWith('pbk_good'));

    expect(result.ok).toBe(true);
    expect(result.steps[1].detail).toContain('skipped');
  });
});
