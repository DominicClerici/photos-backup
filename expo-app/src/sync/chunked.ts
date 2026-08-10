import { Directory, File, Paths } from 'expo-file-system';

import { errorText } from './types';

/** Where the fallback sender stages slices it has to materialize. */
const CHUNK_DIRECTORY = 'chunks';

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
 * Two strategies, because the good one is not known to work. `File.slice()`
 * hands back a Blob backed by native storage, so `fetch` can stream 8MB without
 * it ever existing in JavaScript — but whether React Native's networking accepts
 * that particular Blob is not something the documentation settles, and it is not
 * worth a 100GB backfill finding out the hard way.
 *
 * So the first chunk of a session tries the cheap path. If it fails in a way
 * that looks like "this body type is not supported", the sender switches to
 * staging each slice as a temp file and uploading that — one extra 8MB write and
 * delete per chunk, using the same `File.upload()` call Phase 0 already proved.
 * The choice sticks for the rest of the process.
 */
export class ChunkTransport {
  private strategy: 'blob' | 'staged' | 'unknown' = 'unknown';

  constructor(private readonly onLog?: (line: string) => void) {}

  /** Which path is in use, for the diagnostics screen. */
  get mode(): string {
    return this.strategy;
  }

  async send(url: string, headers: Record<string, string>, chunk: ChunkRef): Promise<Response> {
    if (this.strategy === 'staged') {
      return this.sendStaged(url, headers, chunk);
    }

    try {
      const response = await this.sendBlob(url, headers, chunk);
      if (this.strategy === 'unknown') {
        this.strategy = 'blob';
        // Logged on the way up as well as the way down: which strategy a real
        // phone lands on is the open question this class exists to answer, and
        // silence on success would leave it open.
        this.onLog?.('chunk upload: Blob bodies accepted; slices stream straight from storage');
      }
      return response;
    } catch (e) {
      // A network failure has to stay a network failure — falling back on one
      // would hide a dead server behind a slower code path.
      if (this.strategy !== 'unknown' || !looksLikeUnsupportedBody(e)) {
        throw e;
      }
      this.strategy = 'staged';
      this.onLog?.(`chunk upload: Blob bodies rejected (${errorText(e)}); staging slices to disk instead`);
      return this.sendStaged(url, headers, chunk);
    }
  }

  /** The cheap path: the 8MB never enters the JS heap. */
  private async sendBlob(url: string, headers: Record<string, string>, chunk: ChunkRef): Promise<Response> {
    const blob = chunk.file.slice(chunk.start, chunk.end);
    return fetch(url, { method: 'PUT', headers, body: blob });
  }

  /**
   * The fallback: materialize the slice, upload the file, delete it.
   *
   * The temp file is removed in a finally, and anything that still escapes is
   * cleaned up by sweepChunks() on the next start — a backfill that leaked 8MB
   * per chunk would fill the phone long before it finished.
   */
  private async sendStaged(url: string, headers: Record<string, string>, chunk: ChunkRef): Promise<Response> {
    const directory = chunkDirectory();
    directory.create({ intermediates: true, idempotent: true });

    const staged = new File(directory, `chunk-${chunk.start}.bin`);
    try {
      const buffer = await chunk.file.slice(chunk.start, chunk.end).arrayBuffer();
      if (staged.exists) staged.delete();
      staged.create();
      staged.write(new Uint8Array(buffer));

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
      } catch {
        // Swept on the next run.
      }
    }
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

/**
 * Whether a thrown error means "fetch cannot send this body", as opposed to the
 * network being down.
 *
 * Deliberately generous about what counts: guessing wrong here costs one slower
 * upload path, while guessing the other way costs a video that never uploads.
 */
function looksLikeUnsupportedBody(e: unknown): boolean {
  const message = errorText(e).toLowerCase();
  if (message.includes('network request failed') || message.includes('timed out')) {
    return false;
  }
  return (
    message.includes('blob') ||
    message.includes('body') ||
    message.includes('unsupported') ||
    message.includes('not a function') ||
    message.includes('undefined is not an object') ||
    message.includes('cannot read')
  );
}
