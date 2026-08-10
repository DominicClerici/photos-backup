import { File } from 'expo-file-system';

import { largestVideo } from '../library';
import { note } from '../net';
import { errText, mb, rate, type TestContext, type TestResult } from './types';

/**
 * `File.md5` is a synchronous JSI property, so the hash of a multi-GB video
 * runs to completion on the JS thread. Native and streaming is only half the
 * question — the other half is how long the UI is dead for.
 */
function hashBlocking(file: File) {
  let ticks = 0;
  let maxGap = 0;
  let last = Date.now();
  const id = setInterval(() => {
    const now = Date.now();
    ticks += 1;
    maxGap = Math.max(maxGap, now - last);
    last = now;
  }, 20);

  const t0 = Date.now();
  let md5: string | null = null;
  let error: string | null = null;
  try {
    md5 = file.md5;
  } catch (e) {
    error = errText(e);
  }
  const wallMs = Date.now() - t0;

  const stop = () => {
    clearInterval(id);
    return { ticks, maxGap };
  };
  return { md5, error, wallMs, stop };
}

export async function runMd5(ctx: TestContext): Promise<TestResult> {
  const { log, serverUrl } = ctx;

  log('finding the largest video in the library');
  const target = await largestVideo(10, log);
  if (!target) {
    return { ok: false, verdict: 'no videos in library', details: [] };
  }

  log(`target: ${target.filename} ${mb(target.size)}`);
  log(`uri: ${target.uri}`);

  const file = new File(target.uri);
  if (!file.exists) {
    return {
      ok: false,
      verdict: 'PhotoKit uri is not readable by expo-file-system',
      details: [['uri', target.uri]],
    };
  }

  await note(serverUrl, { phase: 'q1-hash-start', filename: target.filename, size: target.size });

  const run = hashBlocking(file);
  await new Promise((r) => setTimeout(r, 200));
  const { ticks, maxGap } = run.stop();

  if (run.error || !run.md5) {
    return {
      ok: false,
      verdict: `File.md5 threw: ${run.error}`,
      details: [
        ['file', target.filename],
        ['size', mb(target.size)],
        ['uri', target.uri],
      ],
    };
  }

  log(`md5 ${run.md5} in ${run.wallMs}ms (${rate(target.size, run.wallMs)})`);
  log(`js thread: ${ticks} timer ticks during hash, largest gap ${maxGap}ms`);

  log('uploading the same bytes so the server can verify the digest');
  const upload = await file.upload(`${serverUrl}/upload`, {
    httpMethod: 'POST',
    headers: {
      'x-test-id': 'q1-md5',
      'x-client-md5': run.md5,
      'x-filename': target.filename,
      'x-declared-size': String(target.size),
      'content-type': 'application/octet-stream',
    },
  });

  const body = JSON.parse(upload.body) as {
    serverMd5: string;
    md5Match: boolean;
    received: number;
    durationMs: number;
  };
  log(`server md5 ${body.serverMd5} match=${body.md5Match}`);

  return {
    ok: body.md5Match === true,
    verdict: body.md5Match
      ? `native streaming MD5 verified — ${rate(target.size, run.wallMs)}, blocked JS for ${run.wallMs}ms`
      : 'MD5 mismatch between device and server',
    details: [
      ['file', `${target.filename} (${mb(target.size)})`],
      ['uri resolve', `${target.resolveMs} ms`],
      ['hash time', `${run.wallMs} ms`],
      ['throughput', rate(target.size, run.wallMs)],
      ['JS blocked', `${run.wallMs} ms (largest timer gap ${maxGap} ms, ${ticks} ticks)`],
      ['device md5', run.md5],
      ['server md5', body.serverMd5],
      ['bytes received', `${body.received} (${mb(body.received)})`],
      ['upload time', `${body.durationMs} ms`],
    ],
  };
}

/**
 * The legacy path reads the whole file into an NSData before hashing
 * (see ios/Legacy/EXFileSystemLocalFileHandler.m). Kept as a separate,
 * explicitly-opt-in run because a large video is expected to OOM the app.
 */
export async function runLegacyMd5(ctx: TestContext): Promise<TestResult> {
  const { log } = ctx;
  const target = await largestVideo(10, log);
  if (!target) return { ok: false, verdict: 'no videos in library', details: [] };

  log(`legacy getInfoAsync({md5:true}) on ${target.filename} ${mb(target.size)}`);
  log('this loads the entire file into memory — the app may be jetsammed');

  const legacy = await import('expo-file-system/legacy');
  const t0 = Date.now();
  try {
    const info = await legacy.getInfoAsync(target.uri, { md5: true });
    const ms = Date.now() - t0;
    const md5 = 'md5' in info ? (info.md5 as string) : 'none';
    log(`legacy md5 ${md5} in ${ms}ms`);
    return {
      ok: true,
      verdict: `legacy survived ${mb(target.size)} in ${ms}ms — but it buffered the whole file`,
      details: [
        ['file', `${target.filename} (${mb(target.size)})`],
        ['time', `${ms} ms`],
        ['md5', md5],
      ],
    };
  } catch (e) {
    return {
      ok: false,
      verdict: `legacy failed after ${Date.now() - t0}ms: ${errText(e)}`,
      details: [['file', `${target.filename} (${mb(target.size)})`]],
    };
  }
}
