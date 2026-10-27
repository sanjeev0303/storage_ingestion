import { createSHA256 } from "hash-wasm";

export function abortError() {
  return new DOMException("The operation was aborted.", "AbortError");
}

export function isAbort(error: unknown) {
  return error instanceof DOMException && error.name === "AbortError";
}

type HashOptions = { signal?: AbortSignal; onProgress?: (bytesHashed: number) => void };

// Below this a worker costs more to start than the hashing itself.
const WORKER_MIN_BYTES = 16 * 1024 * 1024;

// Hashes a file in chunks as it streams off disk, so a 50 GiB file never sits
// in memory. WebCrypto cannot do this: it only hashes a whole buffer. A large
// one is hashed in a worker, off the thread the upload itself runs on.
export async function sha256Blob(blob: Blob, options: HashOptions = {}): Promise<string> {
  if (typeof Worker === "undefined" || blob.size <= WORKER_MIN_BYTES) return sha256OnThisThread(blob, options);
  const { signal, onProgress } = options;
  if (signal?.aborted) throw abortError();

  const worker = new Worker(new URL("./sha256.worker.ts", import.meta.url), { type: "module" });
  try {
    return await new Promise<string>((resolve, reject) => {
      signal?.addEventListener("abort", () => reject(abortError()), { once: true });
      worker.onerror = () => reject(new Error("The checksum could not be computed in the background."));
      worker.onmessage = (event: MessageEvent<{ progress?: number; sha?: string; error?: string }>) => {
        const { progress, sha, error } = event.data;
        if (error) return reject(new Error(error));
        if (progress !== undefined) onProgress?.(progress);
        if (sha) resolve(sha);
      };
      // A File is passed by reference; nothing is copied.
      worker.postMessage(blob);
    });
  } finally {
    worker.terminate();
  }
}

async function sha256OnThisThread(blob: Blob, options: HashOptions): Promise<string> {
  const { signal, onProgress } = options;
  const hasher = await createSHA256();
  hasher.init();

  const reader = blob.stream().getReader();
  let hashed = 0;
  try {
    for (;;) {
      if (signal?.aborted) throw abortError();
      const { value, done } = await reader.read();
      if (done) break;
      hasher.update(value);
      hashed += value.byteLength;
      onProgress?.(hashed);
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  return hasher.digest("hex");
}
