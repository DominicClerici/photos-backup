/**
 * Sizes and range planning for resumable uploads.
 *
 * Deliberately free of native imports. The sync engine decides which path an
 * original takes and therefore needs these constants, and the engine is
 * testable in plain Node precisely because it never reaches for
 * expo-file-system. The native half lives in chunked.ts.
 */

import { SyncError } from './types';

/**
 * Originals at or above this size go through the resumable path. Below it, a
 * failed upload costs one cheap retry and the bookkeeping is not worth it.
 */
export const CHUNK_THRESHOLD = 64 * 1024 * 1024;

/**
 * Bytes per chunk. Small enough that losing one is nothing, large enough that a
 * 550MB video is ~69 requests rather than thousands.
 */
export const CHUNK_SIZE = 8 * 1024 * 1024;

/** Plans the ranges a resumable upload still owes, given where the server is. */
export function planChunks(
  size: number,
  offset: number,
  chunkSize: number = CHUNK_SIZE
): Array<[number, number]> {
  if (offset < 0 || offset > size) {
    throw new SyncError(`server reported offset ${offset} for a ${size}-byte original`, 'item');
  }

  const ranges: Array<[number, number]> = [];
  for (let start = offset; start < size; start += chunkSize) {
    ranges.push([start, Math.min(start + chunkSize, size)]);
  }
  return ranges;
}
