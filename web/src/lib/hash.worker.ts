/// <reference lib="webworker" />

import { Sha256 } from "./sha256";
import { CHUNK_BYTES, type HashReply, type HashRequest } from "./hashprotocol";

const scope = self as unknown as DedicatedWorkerGlobalScope;

const reply = (message: HashReply) => scope.postMessage(message);

scope.onmessage = async (ev: MessageEvent<HashRequest>) => {
  const { file } = ev.data;
  try {
    const digest = new Sha256();
    for (let at = 0; at < file.size; at += CHUNK_BYTES) {
      const slice = await file.slice(at, at + CHUNK_BYTES).arrayBuffer();
      digest.update(new Uint8Array(slice));
      reply({ read: Math.min(at + CHUNK_BYTES, file.size) });
    }
    reply({ digest: digest.hex() });
  } catch (err) {
    reply({ error: err instanceof Error ? err.message : "could not read the file" });
  }
};
