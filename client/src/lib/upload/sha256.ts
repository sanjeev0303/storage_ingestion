import { createSHA256 } from "hash-wasm";

export function abortError() {
  return new DOMException("The operation was aborted.", "AbortError");
}

export function isAbort(error: unknown) {
  return error instanceof DOMException && error.name === "AbortError";
}

// Hashes a file in chunks as it streams off disk, so a 50 GiB file never sits
// in memory. WebCrypto cannot do this: it only hashes a whole buffer.
export async function sha256Blob(
  blob: Blob,
  options: { signal?: AbortSignal; onProgress?: (bytesHashed: number) => void } = {},
): Promise<string> {
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
