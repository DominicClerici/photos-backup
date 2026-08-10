import { Directory, File, Paths } from 'expo-file-system';
import { AssetField, MediaType, Query } from 'expo-media-library';

import { resolve } from '../library';
import { errText, mb, rate, type TestContext, type TestResult } from './types';

const WORK_DIR = new Directory(Paths.cache, 'phase0');

/**
 * Q1's upload of a PhotoKit original failed while Q3's upload of a file in the
 * app container succeeded. Two differences could explain it: the file lived
 * outside the sandbox, or its URI carried a `#...` fragment. This isolates
 * them one variable at a time.
 */
export async function runUploadSourceMatrix(ctx: TestContext): Promise<TestResult> {
  const { log, serverUrl } = ctx;

  const videos = await new Query()
    .eq(AssetField.MEDIA_TYPE, MediaType.VIDEO)
    .orderBy({ key: AssetField.DURATION, ascending: true })
    .limit(6)
    .exe();
  if (!videos.length) return { ok: false, verdict: 'no videos in library', details: [] };

  let target = await resolve(videos[0]);
  for (const v of videos.slice(1)) {
    const r = await resolve(v);
    if (r.size > 0 && r.size < target.size) target = r;
  }

  const hasFragment = target.uri.includes('#');
  log(`target ${target.filename} ${mb(target.size)} fragment=${hasFragment}`);
  log(`uri ${target.uri}`);

  const stripped = target.uri.split('#')[0];

  if (!WORK_DIR.exists) WORK_DIR.create({ intermediates: true, idempotent: true });
  const copyDest = new File(WORK_DIR, `copy-${target.filename}`);
  if (copyDest.exists) copyDest.delete();

  const copyStart = Date.now();
  await new File(stripped).copy(copyDest);
  const copyMs = Date.now() - copyStart;
  log(`copied into container in ${copyMs}ms (${rate(target.size, copyMs)})`);

  const cases: { name: string; uri: string; sessionType: 'background' | 'foreground' }[] = [
    { name: 'photokit uri, background', uri: target.uri, sessionType: 'background' },
    { name: 'photokit uri, foreground', uri: target.uri, sessionType: 'foreground' },
    { name: 'fragment stripped, background', uri: stripped, sessionType: 'background' },
    { name: 'fragment stripped, foreground', uri: stripped, sessionType: 'foreground' },
    { name: 'copied to container, background', uri: copyDest.uri, sessionType: 'background' },
  ];

  const details: [string, string][] = [
    ['target', `${target.filename} (${mb(target.size)})`],
    ['uri has fragment', String(hasFragment)],
    ['copy into container', `${copyMs} ms (${rate(target.size, copyMs)})`],
  ];

  let anyPhotoKitWorked = false;
  for (const [i, c] of cases.entries()) {
    const file = new File(c.uri);
    const readable = file.exists;
    const t0 = Date.now();
    try {
      const res = await file.upload(`${serverUrl}/upload`, {
        httpMethod: 'POST',
        sessionType: c.sessionType,
        headers: {
          'x-test-id': `q1b-${i}`,
          'x-filename': target.filename,
          'x-declared-size': String(target.size),
          'content-type': 'application/octet-stream',
        },
      });
      const body = JSON.parse(res.body) as { received: number };
      const ms = Date.now() - t0;
      log(`${c.name}: OK ${body.received} bytes in ${ms}ms`);
      details.push([c.name, `OK — ${body.received} bytes in ${ms} ms (exists=${readable})`]);
      if (c.uri !== copyDest.uri) anyPhotoKitWorked = true;
    } catch (e) {
      const ms = Date.now() - t0;
      log(`${c.name}: FAILED after ${ms}ms — ${errText(e)}`);
      details.push([c.name, `FAILED after ${ms} ms (exists=${readable}) — ${errText(e)}`]);
    }
  }

  copyDest.delete();

  return {
    ok: true,
    verdict: anyPhotoKitWorked
      ? 'at least one direct-from-PhotoKit upload worked — see which combination'
      : 'no direct-from-PhotoKit upload worked; originals must be copied into the app container first',
    details,
  };
}
