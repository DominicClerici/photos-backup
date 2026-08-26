import { Sha256 } from "./sha256";
import { CHUNK_BYTES, type HashReply, type HashRequest } from "./hashprotocol";

/**
 * The content key of a file the browser is holding, computed without blocking
 * the page.
 *
 * A worker per file rather than a pool: hashing runs at roughly half a gigabyte
 * a second, so even a long video is a few seconds of one worker's life, and the
 * files are read one after another anyway — a second worker would only make two
 * of them contend for the same disk. Starting one costs a millisecond or two
 * against that.
 *
 * @param onProgress Called with the bytes read so far, for a bar that has to
 * mean something on a file large enough to notice.
 */
export function hashFile(
  file: File,
  onProgress?: (read: number) => void,
  signal?: AbortSignal,
): Promise<string> {
  if (workers !== false) {
    const worker = startWorker();
    if (worker) return hashInWorker(worker, file, onProgress, signal);
  }
  return hashHere(file, onProgress, signal);
}

/**
 * Whether workers are usable here. Unknown until the first one is tried, and
 * false forever once one has failed to start or to load — a browser that cannot
 * run this module in a worker will not be able to on the next file either, and
 * retrying would cost a failed fetch per photograph.
 */
let workers: boolean | undefined = undefined;

function startWorker(): Worker | null {
  try {
    // The `new URL(..., import.meta.url)` form is what tells the bundler this
    // is a worker entry point to compile and emit, rather than a string.
    const worker = new Worker(new URL("./hash.worker.ts", import.meta.url), { type: "module" });
    return worker;
  } catch {
    workers = false;
    return null;
  }
}

function hashInWorker(
  worker: Worker,
  file: File,
  onProgress?: (read: number) => void,
  signal?: AbortSignal,
): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const stop = () => {
      worker.terminate();
      signal?.removeEventListener("abort", abort);
    };
    const abort = () => {
      stop();
      reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
    };
    if (signal?.aborted) return abort();
    signal?.addEventListener("abort", abort);

    worker.onmessage = (ev: MessageEvent<HashReply>) => {
      const reply = ev.data;
      if ("read" in reply) {
        workers = true;
        onProgress?.(reply.read);
        return;
      }
      stop();
      if ("digest" in reply) resolve(reply.digest);
      else reject(new Error(reply.error));
    };

    // A worker that fails to load its module reports it here and never answers,
    // so this is not merely tidiness: without it the first file on a browser
    // that cannot compile the worker would hang instead of falling back.
    worker.onerror = () => {
      stop();
      // Only when nothing has come back yet. A failure after the first progress
      // message is a failure of this file, not of workers.
      if (workers === undefined) {
        workers = false;
        hashHere(file, onProgress, signal).then(resolve, reject);
        return;
      }
      reject(new Error("the file could not be read"));
    };

    const request: HashRequest = { file };
    worker.postMessage(request);
  });
}

/** The same walk on the main thread, for a browser with no workers. */
async function hashHere(
  file: File,
  onProgress?: (read: number) => void,
  signal?: AbortSignal,
): Promise<string> {
  const digest = new Sha256();
  for (let at = 0; at < file.size; at += CHUNK_BYTES) {
    signal?.throwIfAborted();
    const slice = await file.slice(at, at + CHUNK_BYTES).arrayBuffer();
    digest.update(new Uint8Array(slice));
    onProgress?.(Math.min(at + CHUNK_BYTES, file.size));
  }
  return digest.hex();
}
