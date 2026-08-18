import { File } from 'expo-file-system';
import { Asset } from 'expo-media-library';

export type UploadStage = 'resolving' | 'hashing' | 'uploading' | 'done';

export type UploadResult = {
  id: string;
  sha256: string;
  duplicate: boolean;
  filename: string;
  size: number;
  hashMs: number;
  uploadMs: number;
};

export class UploadError extends Error {
  constructor(
    message: string,
    readonly stage: UploadStage
  ) {
    super(message);
  }
}

type Options = {
  serverUrl: string;
  deviceId: string;
  onStage: (stage: UploadStage) => void;
};

/**
 * Sends one original straight from PhotoKit.
 *
 * Two Phase 0 findings shape this. `sessionType` must be 'foreground': a
 * background NSURLSession hands the file to nsurlsessiond, which does not
 * inherit the app's PhotoKit sandbox extension and fails before a byte leaves
 * the device. And `File.md5` is a synchronous JSI property that blocks the JS
 * thread for the length of the hash, so the caller is told to expect a freeze.
 */
export async function uploadAsset(assetId: string, opts: Options): Promise<UploadResult> {
  const { serverUrl, deviceId, onStage } = opts;

  onStage('resolving');
  const asset = new Asset(assetId);
  let uri: string;
  let filename: string;
  let createdAt: number | null;
  try {
    [uri, filename, createdAt] = await Promise.all([
      asset.getUri(),
      asset.getFilename(),
      asset.getCreationTime(),
    ]);
  } catch (e) {
    throw new UploadError(errorText(e), 'resolving');
  }

  const file = new File(uri);
  if (!file.exists) {
    throw new UploadError(`file not readable at ${uri}`, 'resolving');
  }
  const size = file.size ?? 0;

  onStage('hashing');
  // Let React paint the "hashing" state before the JS thread stalls.
  await new Promise((resolve) => setTimeout(resolve, 50));
  const hashStart = Date.now();
  let md5: string | null;
  try {
    md5 = file.md5;
  } catch (e) {
    throw new UploadError(errorText(e), 'hashing');
  }
  const hashMs = Date.now() - hashStart;
  if (!md5) {
    throw new UploadError('could not hash the original', 'hashing');
  }

  onStage('uploading');
  const uploadStart = Date.now();
  const headers: Record<string, string> = {
    'content-type': 'application/octet-stream',
    'x-photo-filename': filename,
    'x-photo-md5': md5,
    'x-photo-size': String(size),
    'x-photo-device-id': deviceId,
    'x-photo-local-id': assetId,
  };
  if (createdAt) headers['x-photo-captured-at'] = new Date(createdAt).toISOString();

  let response: { status: number; body: string };
  try {
    response = await file.upload(`${serverUrl}/v1/assets`, {
      httpMethod: 'POST',
      sessionType: 'foreground',
      headers,
    });
  } catch (e) {
    throw new UploadError(errorText(e), 'uploading');
  }
  const uploadMs = Date.now() - uploadStart;

  if (response.status < 200 || response.status >= 300) {
    throw new UploadError(`server returned ${response.status}: ${response.body}`, 'uploading');
  }

  let parsed: { id: string; sha256: string; duplicate: boolean };
  try {
    parsed = JSON.parse(response.body);
  } catch {
    throw new UploadError(`unreadable server response: ${response.body}`, 'uploading');
  }

  onStage('done');
  return { ...parsed, filename, size, hashMs, uploadMs };
}

export function errorText(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}
