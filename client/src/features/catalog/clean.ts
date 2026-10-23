// Paths and ids as people paste them: stray spaces, quotes, a leading slash.

const MAX_PATH_BYTES = 1024;

export function cleanPath(value: string) {
  return value
    .trim()
    .replace(/^["'`]+|["'`]+$/g, "")
    .replace(/^\/+/, "")
    .trim();
}

export function pathProblem(path: string): string | null {
  if (new TextEncoder().encode(path).length > MAX_PATH_BYTES) return "A path can be at most 1024 bytes.";
  return null;
}
