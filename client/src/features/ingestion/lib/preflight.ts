export const MAX_LINE_BYTES = 16384;
export const MAX_SAMPLES = 50;
export const MAX_TRACKED_PATHS = 200_000;

export type PreflightSample = { line: number; problem: string };

export type PreflightReport = {
  fileBytes: number;
  lines: number;
  blankLines: number;
  problemLines: number;
  samples: PreflightSample[];
  totalEntryBytes: number;
  duplicatePaths: number;
  // False once the file had more paths than the browser tracks.
  duplicatesFullyChecked: boolean;
};

type Limits = { maxEntries?: number; maxBytes?: number };

export function exceededLimits(report: PreflightReport, limits: Limits) {
  const entries = Boolean(limits.maxEntries) && report.lines > (limits.maxEntries ?? 0);
  const bytes = Boolean(limits.maxBytes) && report.fileBytes > (limits.maxBytes ?? 0);
  return { entries, bytes };
}

const CHECKSUM = /^sha256:[0-9a-f]{64}$/i;
const encoder = new TextEncoder();

function pathKey(path: string) {
  return path
    .split("/")
    .filter((part) => part !== "" && part !== ".")
    .join("/")
    .toLowerCase();
}

function checkLine(text: string): { problem?: string; path?: string; size?: number } {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return { problem: "Not valid JSON (invalid_json)" };
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return { problem: "The line must be a JSON object (invalid_json)" };
  }
  const line = value as Record<string, unknown>;

  const path = line.path;
  if (typeof path !== "string" || path.trim() === "") return { problem: "Missing path (invalid_path)" };
  if (path.startsWith("/") || path.includes("\\") || path.split("/").includes("..")) {
    return { problem: "Path must be relative, without backslashes or .. segments (invalid_path)", path };
  }

  const size = line.size;
  if (size === undefined && "size_bytes" in line) {
    return { problem: 'The key is "size", not "size_bytes" (invalid_size)', path };
  }
  if (typeof size !== "number" || !Number.isInteger(size) || size < 1) {
    return { problem: 'Missing or invalid "size": a whole number of bytes, 1 or more (invalid_size)', path };
  }

  if (typeof line.content_type !== "string" || line.content_type.trim() === "") {
    return { problem: "Missing content_type (invalid_content_type)", path, size };
  }

  if (line.checksum === undefined) return { problem: "Missing checksum (checksum_required)", path, size };
  if (typeof line.checksum !== "string" || !CHECKSUM.test(line.checksum)) {
    return { problem: 'checksum must be "sha256:" followed by 64 hex characters (invalid_checksum)', path, size };
  }

  if (line.required !== undefined && typeof line.required !== "boolean") {
    return { problem: "required must be true or false", path, size };
  }
  const metadata = line.metadata;
  if (metadata !== undefined) {
    const flat =
      typeof metadata === "object" &&
      metadata !== null &&
      !Array.isArray(metadata) &&
      Object.values(metadata).every((v) => typeof v === "string");
    if (!flat) return { problem: "metadata must be an object of string values (invalid_metadata)", path, size };
  }
  return { path, size };
}

// Reads the manifest as a stream, one line at a time. Only counters, the
// first 50 problems and a bounded set of paths stay in memory.
export async function preflightManifest(
  file: Blob,
  options: { signal?: AbortSignal; onProgress?: (report: PreflightReport) => void } = {},
): Promise<PreflightReport> {
  const report: PreflightReport = {
    fileBytes: file.size,
    lines: 0,
    blankLines: 0,
    problemLines: 0,
    samples: [],
    totalEntryBytes: 0,
    duplicatePaths: 0,
    duplicatesFullyChecked: true,
  };
  const seen = new Set<string>();
  let lineNumber = 0;

  function note(problem: string) {
    report.problemLines++;
    if (report.samples.length < MAX_SAMPLES) report.samples.push({ line: lineNumber, problem });
  }

  function finishLine(text: string, tooLong: boolean) {
    lineNumber++;
    if (!tooLong && text.trim() === "") {
      report.blankLines++;
      return;
    }
    report.lines++;
    if (tooLong || encoder.encode(text).length > MAX_LINE_BYTES) {
      note(`Longer than ${MAX_LINE_BYTES} bytes (line_too_long)`);
      return;
    }
    const result = checkLine(text);
    if (result.problem) {
      note(result.problem);
      return;
    }
    report.totalEntryBytes += result.size ?? 0;
    const key = pathKey(result.path ?? "");
    if (seen.has(key)) {
      report.duplicatePaths++;
      note(`Duplicate path "${result.path}" (duplicate_path)`);
    } else if (seen.size < MAX_TRACKED_PATHS) {
      seen.add(key);
    } else {
      report.duplicatesFullyChecked = false;
    }
  }

  const reader = file.stream().pipeThrough(new TextDecoderStream()).getReader();
  let pending = "";
  let tooLong = false;
  let lastReport = 0;

  try {
    for (;;) {
      if (options.signal?.aborted) throw new DOMException("The operation was aborted.", "AbortError");
      const { value, done } = await reader.read();
      if (done) break;

      let start = 0;
      for (let end = value.indexOf("\n", start); end !== -1; end = value.indexOf("\n", start)) {
        if (!tooLong) pending += value.slice(start, end);
        finishLine(pending.replace(/\r$/, ""), tooLong);
        pending = "";
        tooLong = false;
        start = end + 1;
      }
      if (!tooLong) pending += value.slice(start);
      // More characters than the byte limit means more bytes than it too, so stop keeping the line.
      if (pending.length > MAX_LINE_BYTES) {
        tooLong = true;
        pending = "";
      }

      if (options.onProgress && lineNumber - lastReport >= 5000) {
        lastReport = lineNumber;
        options.onProgress({ ...report, samples: [...report.samples] });
      }
    }
    if (pending !== "" || tooLong) finishLine(pending.replace(/\r$/, ""), tooLong);
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  return report;
}
