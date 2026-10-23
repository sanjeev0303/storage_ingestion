export const MAX_KEYS = 64;
const KEY_PATTERN = /^[A-Za-z0-9._-]{1,64}$/;

export function validateKey(key: string): string | null {
  if (!key) return "Enter a key.";
  if (!KEY_PATTERN.test(key)) return "Use 1 to 64 letters, digits, dots, dashes or underscores.";
  if (key.startsWith("system.")) return "Keys starting with system. are reserved.";
  return null;
}

export function validateValue(value: string): string | null {
  if (new TextEncoder().encode(value).length > 1024) return "A value can be at most 1024 bytes.";
  return null;
}

// Builds the merge patch: changed or new keys carry the string, removed keys carry null.
export function buildPatch(
  original: Record<string, string>,
  edited: Record<string, string>,
): Record<string, string | null> {
  const patch: Record<string, string | null> = {};
  for (const key of Object.keys(original)) {
    if (!(key in edited)) patch[key] = null;
  }
  for (const [key, value] of Object.entries(edited)) {
    if (original[key] !== value) patch[key] = value;
  }
  return patch;
}
