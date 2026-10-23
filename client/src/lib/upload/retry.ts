import { abortError } from "./sha256";

type RetryOptions = {
  retries?: number;
  baseMs?: number;
  maxMs?: number;
  signal?: AbortSignal;
  shouldRetry?: (error: unknown) => boolean;
  // Called before each wait; the attempt number starts at 1.
  onRetry?: (error: unknown, attempt: number, waitMs: number) => void;
  // Lets a test run without real waiting.
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
};

export function sleep(ms: number, signal?: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) return reject(abortError());
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(abortError());
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

// Exponential backoff with jitter. Never retries an abort.
export async function withRetry<T>(run: (attempt: number) => Promise<T>, options: RetryOptions = {}): Promise<T> {
  const { retries = 4, baseMs = 500, maxMs = 8000, signal, shouldRetry = () => true, onRetry, sleep: wait = sleep } = options;

  for (let attempt = 0; ; attempt++) {
    if (signal?.aborted) throw abortError();
    try {
      return await run(attempt);
    } catch (error) {
      if (signal?.aborted || (error instanceof DOMException && error.name === "AbortError")) throw error;
      if (attempt >= retries || !shouldRetry(error)) throw error;
      const waitMs = Math.min(maxMs, baseMs * 2 ** attempt) * (0.7 + Math.random() * 0.6);
      onRetry?.(error, attempt + 1, waitMs);
      await wait(waitMs, signal);
    }
  }
}
