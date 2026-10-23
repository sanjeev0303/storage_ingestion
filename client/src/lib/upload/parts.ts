export const MIB = 1024 * 1024;
export const MIN_PART_BYTES = 5 * MIB;
export const MAX_PARTS = 10_000;

export type Part = { number: number; start: number; end: number; size: number };

// The API leaves part size to the client. Every part but the last must be at
// least 5 MiB and there can be at most 10,000, so large files need bigger parts.
export function choosePartSize(totalBytes: number, preferredBytes = 8 * MIB) {
  const needed = Math.ceil(totalBytes / MAX_PARTS);
  const bytes = Math.max(preferredBytes, MIN_PART_BYTES, needed);
  return Math.ceil(bytes / MIB) * MIB;
}

export function planParts(totalBytes: number, partSize: number): Part[] {
  const parts: Part[] = [];
  for (let start = 0, number = 1; start < totalBytes; start += partSize, number++) {
    const end = Math.min(start + partSize, totalBytes);
    parts.push({ number, start, end, size: end - start });
  }
  return parts;
}

// Runs the worker over items with at most `limit` in flight. The first
// failure stops new work and is rethrown once running work has settled.
export async function runPool<T>(
  items: readonly T[],
  limit: number,
  worker: (item: T, index: number) => Promise<void>,
  signal?: AbortSignal,
) {
  let next = 0;
  let failure: unknown;
  let failed = false;

  async function lane() {
    while (!failed && !signal?.aborted) {
      const index = next++;
      if (index >= items.length) return;
      try {
        await worker(items[index], index);
      } catch (error) {
        if (!failed) {
          failed = true;
          failure = error;
        }
      }
    }
  }

  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, lane));
  if (failed) throw failure;
}
