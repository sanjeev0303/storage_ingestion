const DATASET = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const METADATA_KEY = /^[A-Za-z0-9._-]{1,64}$/;
const BAD_PATH_CHARS = /[<>:"|?*\\\u0000-\u001f\u007f]/;
const RESERVED_TOP = ["_system", "_ingestion", ".well-known"];
const RESERVED_KEYS = ["id", "tenant_id", "path", "status", "size_bytes", "checksum_sha256", "retention", "legal_hold"];

const byteLength = (text: string) => new TextEncoder().encode(text).length;

export function validateDataset(value: string): string | null {
  if (!value) return null;
  if (DATASET.test(value)) return null;
  return "Use 1 to 64 characters: lowercase letters, digits, dot, dash or underscore, starting with a letter or digit.";
}

export function validatePath(value: string): string | null {
  if (!value) return "Enter a catalog path.";
  if (byteLength(value) > 1024) return "The path can be at most 1024 bytes.";
  if (value.startsWith("/")) return "The path cannot start with a slash.";
  if (BAD_PATH_CHARS.test(value)) return "The path cannot contain \\ < > : \" | ? * or control characters.";

  const segments = value.split("/");
  if (segments.length > 64) return "The path can be at most 64 folders deep.";
  for (const segment of segments) {
    if (!segment) return "The path cannot have an empty folder name.";
    if (segment === "..") return "The path cannot contain \"..\".";
    if (segment !== segment.trim()) return "Folder and file names cannot start or end with a space.";
    if (byteLength(segment) > 255) return "Each folder or file name can be at most 255 bytes.";
  }
  if (RESERVED_TOP.includes(segments[0].toLowerCase())) return `The top folder "${segments[0]}" is reserved.`;
  return null;
}

// One "key=value" per line. Returns the parsed map or the first problem found.
export function parseMetadata(text: string): { value: Record<string, string>; error: string | null } {
  const value: Record<string, string> = {};
  let total = 0;

  for (const rawLine of text.split("\n")) {
    const line = rawLine.trim();
    if (!line) continue;

    const at = line.indexOf("=");
    if (at < 1) return { value, error: `"${line}" needs the form key=value.` };

    const key = line.slice(0, at).trim();
    const val = line.slice(at + 1).trim();
    if (!METADATA_KEY.test(key)) {
      return { value, error: `"${key}" is not a valid key: use letters, digits, dot, dash or underscore, up to 64.` };
    }
    if (key.toLowerCase().startsWith("system.") || RESERVED_KEYS.includes(key.toLowerCase())) {
      return { value, error: `"${key}" is reserved by the platform.` };
    }
    if (key in value) return { value, error: `"${key}" appears twice.` };
    if (byteLength(val) > 1024) return { value, error: `The value for "${key}" is over 1024 bytes.` };

    value[key] = val;
    total += byteLength(key) + byteLength(val);
  }

  if (Object.keys(value).length > 64) return { value, error: "Use at most 64 metadata keys." };
  if (total > 8192) return { value, error: "Metadata can be at most 8192 bytes in total." };
  return { value, error: null };
}
