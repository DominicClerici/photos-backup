import { Directory, File, FileMode, Paths } from 'expo-file-system';

import { SyncError } from './types';

/** Where the sender stages the slice it is about to upload. */
const CHUNK_DIRECTORY = 'chunks';

/**
 * How much of a chunk crosses the JS heap at a time while it is staged.
 *
 * The chunk itself is 8MB, but there is no reason to hold all of it: the copy
 * reads a block, writes it, and drops it, so a backfill of 550MB videos never
 * has more than this in JavaScript at once.
 */
const COPY_BLOCK = 1024 * 1024;

export type ChunkSender = (url: string, headers: Record<string, string>, chunk: ChunkRef) => Promise<Response>;

/** One byte range of a local original, not yet read. */
export type ChunkRef = {
  file: File;
  start: number;
  end: number;
};

/**
 * How chunks get onto the wire.
 *
 * The obvious implementation — `fetch(url, { body: file.slice(start, end) })` —
 * cannot work on this stack, in two separate ways. expo-file-system 57
 * implements `File.slice()` as `new Blob([this.bytesSync().slice(start, end)])`,
 * which first reads the *entire* original into JavaScript (550MB of video, to
 * send 8MB of it) and then hands React Native's BlobManager a typed array, which
 * it refuses outright: "Creating blobs from 'ArrayBuffer' and 'ArrayBufferView'
 * are not supported". Nothing about that is conditional or version-dependent, so
 * there is no cheap path to try first and no runtime question to answer.
 *
 * What is left is the path Phase 0 already proved: stage the range as a temp
 * file and hand it to `File.upload()`, which streams from native storage. The
 * copy costs one 8MB write and delete per chunk and never opens the original in
 * JavaScript — only `COPY_BLOCK` bytes of it exist here at a time.
 */
export class ChunkTransport {
  constructor(private readonly onLog?: (line: string) => void) {}

  /** Which path is in use, for the diagnostics screen. */
  get mode(): string {
    return 'staged';
  }

  async send(url: string, headers: Record<string, string>, chunk: ChunkRef): Promise<Response> {
    const directory = chunkDirectory();
    directory.create({ intermediates: true, idempotent: true });

    const staged = new File(directory, `chunk-${chunk.start}.bin`);
    try {
      stage(chunk, staged);

      const result = await staged.upload(url, {
        httpMethod: 'PUT',
        sessionType: 'foreground',
        headers,
      });
      // File.upload resolves for any completed response, including non-2xx, so
      // this is shaped back into something the caller can read like a fetch.
      return new Response(result.body, { status: result.status });
    } finally {
      try {
        if (staged.exists) staged.delete();
      } catch (e) {
        // Swept on the next run; it costs disk, not correctness.
        this.onLog?.(`could not remove a staged chunk: ${String(e)}`);
      }
    }
  }
}

/**
 * Copies one byte range of the original into `staged`, block by block.
 *
 * Both halves are native file handles, so the original is never opened as a
 * whole — the alternative is what `File.slice()` does, and a 550MB read on a
 * phone is a crash rather than a slow path.
 */
function stage(chunk: ChunkRef, staged: File): void {
  if (staged.exists) staged.delete();
  staged.create();

  const source = chunk.file.open(FileMode.ReadOnly);
  try {
    source.offset = chunk.start;
    const sink = staged.open(FileMode.WriteOnly);
    try {
      for (let at = chunk.start; at < chunk.end; ) {
        const block = source.readBytes(Math.min(COPY_BLOCK, chunk.end - at));
        // A read that returns nothing before the range is filled means the
        // original is shorter than the size the upload was declared with, which
        // no amount of retrying against the server fixes.
        if (block.length === 0) {
          throw new SyncError(
            `${chunk.file.uri} ended at ${at} bytes, short of the ${chunk.end} the upload declared`,
            'item'
          );
        }
        sink.writeBytes(block);
        at += block.length;
      }
    } finally {
      sink.close();
    }
  } finally {
    source.close();
  }
}

/** Removes staged chunks a previous run left behind. */
export function sweepChunks(): number {
  const directory = chunkDirectory();
  if (!directory.exists) return 0;

  let removed = 0;
  for (const entry of directory.list()) {
    if (!(entry instanceof File)) continue;
    try {
      entry.delete();
      removed += 1;
    } catch {
      // Nothing to do; it costs disk, not correctness.
    }
  }
  return removed;
}

function chunkDirectory(): Directory {
  return new Directory(Paths.cache, CHUNK_DIRECTORY);
}
