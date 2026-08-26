/**
 * The one thing the page and its hashing worker both have to agree on.
 *
 * Separate from hash.ts so that importing the protocol does not drag the code
 * that constructs a Worker into the worker's own bundle.
 */

/**
 * How much of a file is read at a time.
 *
 * Four megabytes is a compromise between two costs that pull opposite ways: a
 * smaller slice means more round trips through the file reader, and a larger
 * one means a bigger copy resident in memory for every file being hashed at
 * once. It is also roughly one progress update per 8ms of hashing, which is
 * about as often as a bar is worth redrawing.
 */
export const CHUNK_BYTES = 4 << 20;

export interface HashRequest {
  file: File;
}

/** Progress, then exactly one of a digest or an error. */
export type HashReply = { read: number } | { digest: string } | { error: string };
