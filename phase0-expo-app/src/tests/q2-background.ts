import { AppState } from 'react-native';
import { Directory, File, Paths, type UploadTask } from 'expo-file-system';

import { fetchTimeline, note, resetTimeline } from '../net';
import { errText, mb, rate, type TestContext, type TestResult } from './types';

const WORK_DIR = new Directory(Paths.cache, 'phase0');

export type BackgroundOptions = {
  sizeMb: number;
  throttleBytesPerSec: number;
  sessionType: 'background' | 'foreground';
};

/**
 * A synthetic file rather than a real video: the point of Q2 is the transfer's
 * behaviour across suspension, and a fixed size plus a server-side throttle
 * makes the window to background the app predictable.
 */
export function ensureSyntheticFile(sizeMb: number, log: (m: string) => void): File {
  if (!WORK_DIR.exists) WORK_DIR.create({ intermediates: true, idempotent: true });
  const file = new File(WORK_DIR, `payload-${sizeMb}mb.bin`);
  if (file.exists && file.size === sizeMb * 1024 * 1024) {
    log(`reusing ${file.name} (${mb(file.size)})`);
    return file;
  }
  if (file.exists) file.delete();
  file.create({ overwrite: true });

  const chunk = new Uint8Array(1024 * 1024);
  for (let i = 0; i < chunk.length; i++) chunk[i] = (i * 31 + 7) & 0xff;
  for (let i = 0; i < sizeMb; i++) {
    chunk[0] = i & 0xff;
    file.write(chunk, { append: true });
  }
  log(`created ${file.name} (${mb(file.size)})`);
  return file;
}

let activeTask: UploadTask | null = null;

export function cancelActive() {
  activeTask?.cancel();
  activeTask = null;
}

export async function runBackgroundUpload(
  ctx: TestContext,
  opts: BackgroundOptions
): Promise<TestResult> {
  const { log, serverUrl } = ctx;
  const testId = `q2-${opts.sessionType}`;

  await resetTimeline(serverUrl);
  const file = ensureSyntheticFile(opts.sizeMb, log);

  const t0 = Date.now();
  const clientMd5 = file.md5;
  log(`md5 ${clientMd5} (${Date.now() - t0}ms)`);

  const expectedSeconds = file.size / opts.throttleBytesPerSec;
  log(
    `uploading ${mb(file.size)} at ~${(opts.throttleBytesPerSec / 1024).toFixed(0)} KB/s ` +
      `= ~${expectedSeconds.toFixed(0)}s, sessionType=${opts.sessionType}`
  );
  log('>>> press the Home button now, wait, then come back <<<');

  await note(serverUrl, {
    phase: 'q2-start',
    sessionType: opts.sessionType,
    size: file.size,
    clientMd5,
    appState: AppState.currentState,
  });

  let lastProgressLog = 0;
  let lastBytesSent = 0;
  let progressEvents = 0;

  const url = `${serverUrl}/upload?throttle=${opts.throttleBytesPerSec}`;
  const task = file.createUploadTask(url, {
    httpMethod: 'POST',
    sessionType: opts.sessionType,
    headers: {
      'x-test-id': testId,
      'x-client-md5': clientMd5 ?? '',
      'x-filename': file.name,
      'x-declared-size': String(file.size),
      'content-type': 'application/octet-stream',
    },
    onProgress: ({ bytesSent, totalBytes }) => {
      progressEvents += 1;
      lastBytesSent = bytesSent;
      const now = Date.now();
      if (now - lastProgressLog < 1000) return;
      lastProgressLog = now;
      log(
        `progress ${((bytesSent / totalBytes) * 100).toFixed(1)}% ` +
          `(${bytesSent}/${totalBytes}) appState=${AppState.currentState}`
      );
    },
  });
  activeTask = task;

  const startedAt = Date.now();
  try {
    const result = await task.uploadAsync();
    const elapsed = Date.now() - startedAt;
    activeTask = null;

    const body = JSON.parse(result.body) as { serverMd5: string; md5Match: boolean; received: number };
    log(`upload finished in ${elapsed}ms, server md5 match=${body.md5Match}`);
    await note(serverUrl, { phase: 'q2-resolved', elapsed, appState: AppState.currentState });

    const timeline = await fetchTimeline(serverUrl);
    const gaps = describeGaps(timeline);

    return {
      ok: body.md5Match === true,
      verdict: body.md5Match
        ? `completed in ${(elapsed / 1000).toFixed(0)}s — check the gap column against when you backgrounded`
        : 'upload completed but the digest did not match',
      details: [
        ['session type', opts.sessionType],
        ['payload', `${mb(file.size)} @ ${(opts.throttleBytesPerSec / 1024).toFixed(0)} KB/s`],
        ['elapsed', `${(elapsed / 1000).toFixed(1)} s`],
        ['effective rate', rate(file.size, elapsed)],
        ['JS progress events', String(progressEvents)],
        ['last bytesSent seen by JS', String(lastBytesSent)],
        ['server md5 match', String(body.md5Match)],
        ['bytes at server', `${body.received} (${mb(body.received)})`],
        ['server tick gaps', gaps],
      ],
    };
  } catch (e) {
    activeTask = null;
    const elapsed = Date.now() - startedAt;
    log(`uploadAsync rejected after ${elapsed}ms: ${errText(e)}`);
    await note(serverUrl, { phase: 'q2-rejected', elapsed, error: errText(e) });

    const timeline = await fetchTimeline(serverUrl);
    const received = lastServerBytes(timeline);

    return {
      ok: false,
      verdict: `uploadAsync rejected after ${(elapsed / 1000).toFixed(0)}s — but the server may still have received bytes`,
      details: [
        ['session type', opts.sessionType],
        ['error', errText(e)],
        ['JS progress events', String(progressEvents)],
        ['bytes at server when it stopped', String(received)],
        ['server tick gaps', describeGaps(timeline)],
      ],
    };
  }
}

function lastServerBytes(timeline: Awaited<ReturnType<typeof fetchTimeline>>) {
  const ticks = timeline.filter((e) => e.event === 'upload-tick' || e.event === 'upload-complete');
  const last = ticks[ticks.length - 1];
  return last ? Number(last.detail.received ?? 0) : 0;
}

/**
 * The interesting signal is whether byte arrival *paused* while the app was
 * suspended, so summarise the ticks where nothing came in.
 */
function describeGaps(timeline: Awaited<ReturnType<typeof fetchTimeline>>) {
  const ticks = timeline.filter((e) => e.event === 'upload-tick');
  if (!ticks.length) return 'no ticks recorded';
  const stalled = ticks.filter((t) => Number(t.detail.deltaBytes ?? 0) === 0).length;
  return `${ticks.length} ticks, ${stalled} with zero bytes`;
}
