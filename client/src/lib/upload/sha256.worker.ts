/// <reference lib="webworker" />
import { createSHA256 } from "hash-wasm";

// Hashes the Blob it is sent off the page's thread, so a large file's
// checksum never competes with its own upload for the main thread. Progress
// goes back every 8 MiB, then the hex digest.
const PROGRESS_EVERY = 8 * 1024 * 1024;

self.onmessage = async (event: MessageEvent<Blob>) => {
  try {
    const hasher = await createSHA256();
    hasher.init();
    const reader = event.data.stream().getReader();
    let hashed = 0;
    let reported = 0;
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      hasher.update(value);
      hashed += value.byteLength;
      if (hashed - reported >= PROGRESS_EVERY) {
        reported = hashed;
        self.postMessage({ progress: hashed });
      }
    }
    self.postMessage({ progress: hashed, sha: hasher.digest("hex") });
  } catch (error) {
    self.postMessage({ error: error instanceof Error ? error.message : String(error) });
  }
};
