import { File } from 'expo-file-system';
import { AssetField, MediaSubtype, MediaType, Query, type Asset } from 'expo-media-library';

import { resolve } from '../library';
import { errText, mb, type TestContext, type TestResult } from './types';

const SCAN_LIMIT = 400;
const WANTED = 3;

async function findLivePhotos(log: (m: string) => void) {
  const photos = await new Query()
    .eq(AssetField.MEDIA_TYPE, MediaType.IMAGE)
    .orderBy({ key: AssetField.CREATION_TIME, ascending: false })
    .limit(SCAN_LIMIT)
    .exe();

  log(`scanning ${photos.length} photos for the livePhoto subtype`);
  const found: Asset[] = [];
  let scanned = 0;
  const t0 = Date.now();
  for (const asset of photos) {
    scanned += 1;
    const subtypes = await asset.getMediaSubtypes();
    if (subtypes.includes(MediaSubtype.LIVE_PHOTO)) found.push(asset);
    if (found.length >= WANTED) break;
  }
  log(`found ${found.length} after ${scanned} assets (${Date.now() - t0}ms)`);
  return { found, scanned, scanMs: Date.now() - t0 };
}

export async function runLivePhoto(ctx: TestContext): Promise<TestResult> {
  const { log, serverUrl } = ctx;

  const { found, scanned, scanMs } = await findLivePhotos(log);
  if (!found.length) {
    return {
      ok: false,
      verdict: `no Live Photos among the ${scanned} most recent photos`,
      details: [['scanned', String(scanned)]],
    };
  }

  const details: [string, string][] = [
    ['subtype scan', `${scanned} assets in ${scanMs} ms (${(scanMs / scanned).toFixed(1)} ms each)`],
  ];
  let allOk = true;

  for (const [i, asset] of found.entries()) {
    const label = `live ${i + 1}`;
    try {
      const still = await resolve(asset);
      log(`${label}: still ${still.filename} ${mb(still.size)}`);

      const t0 = Date.now();
      const videoUri = await asset.getLivePhotoVideoUri();
      const extractMs = Date.now() - t0;

      if (!videoUri) {
        allOk = false;
        details.push([label, `${still.filename} — getLivePhotoVideoUri() returned null`]);
        continue;
      }

      const video = new File(videoUri);
      const exists = video.exists;
      const size = exists ? video.size : 0;
      log(`${label}: paired ${video.name} ${mb(size)} extracted in ${extractMs}ms`);
      log(`${label}: at ${videoUri}`);

      if (!exists || size === 0) {
        allOk = false;
        details.push([label, `${still.filename} — uri returned but file is empty`]);
        continue;
      }

      const md5 = video.md5;
      const upload = await video.upload(`${serverUrl}/upload`, {
        httpMethod: 'POST',
        headers: {
          'x-test-id': `q3-live-${i + 1}`,
          'x-client-md5': md5 ?? '',
          'x-filename': video.name,
          'x-declared-size': String(size),
          'content-type': 'video/quicktime',
        },
      });
      const body = JSON.parse(upload.body) as { md5Match: boolean; received: number };
      if (!body.md5Match) allOk = false;
      log(`${label}: uploaded ${body.received} bytes, md5Match=${body.md5Match}`);

      details.push([
        label,
        `${still.filename} (${mb(still.size)}) + ${video.name} (${mb(size)}) — ` +
          `extract ${extractMs} ms, upload md5Match=${body.md5Match}`,
      ]);
      // The paired video is written to NSTemporaryDirectory by the module and
      // is never cleaned up, so v1 has to delete it after upload.
      details.push([`${label} temp path`, videoUri]);
    } catch (e) {
      allOk = false;
      log(`${label}: ${errText(e)}`);
      details.push([label, `failed: ${errText(e)}`]);
    }
  }

  return {
    ok: allOk,
    verdict: allOk
      ? `paired .mov retrieved and verified for ${found.length} Live Photo(s) — no native module needed`
      : 'at least one Live Photo could not be paired',
    details,
  };
}
