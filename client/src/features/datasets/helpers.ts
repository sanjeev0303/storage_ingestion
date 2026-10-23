import { describeError, isApiError } from "@/lib/api/errors";
import { splitList } from "@/lib/utils/forgive";
import type { LifecycleStatus, MemberRequest, Split, ValidationStatus } from "./types";

export const NAME_PATTERN = /^[a-z0-9][a-z0-9._-]{0,63}$/;
export const TAG_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$/;
export const SPLITS: Split[] = ["train", "validation", "test"];

export const INLINE_LIMIT_BYTES = 8 * 1024 * 1024;
export const MAX_LINE_BYTES = 64 * 1024;
export const MAX_ERROR_SAMPLES = 50;

export const lifecycleLabels: Record<LifecycleStatus, string> = {
  DRAFT: "Draft",
  VALIDATING: "Validating",
  READY: "Ready",
  READY_WITH_WARNINGS: "Ready with warnings",
  FAILED: "Failed validation",
  ARCHIVED: "Archived",
};

export const validationLabels: Record<ValidationStatus, string> = {
  NOT_VALIDATED: "Not validated",
  READY: "Ready",
  READY_WITH_WARNINGS: "Ready with warnings",
  FAILED: "Failed validation",
};

export function lifecycleTone(status: LifecycleStatus) {
  if (status === "DRAFT") return "neutral" as const;
  if (status === "VALIDATING") return "active" as const;
  if (status === "FAILED") return "attention" as const;
  return "done" as const;
}

export function validationTone(status: ValidationStatus) {
  if (status === "NOT_VALIDATED") return "neutral" as const;
  if (status === "FAILED") return "attention" as const;
  return "done" as const;
}

export function validateName(name: string) {
  if (!name) return "Enter a name.";
  if (!NAME_PATTERN.test(name)) {
    return "Use 1 to 64 characters: lowercase letters, digits, dot, underscore or hyphen, starting with a letter or digit.";
  }
  return null;
}

export function parseTags(text: string) {
  const tags = splitList(text);
  const bad = tags.find((tag) => !TAG_PATTERN.test(tag));
  return { tags, error: bad ? `"${bad}" is not a valid tag. Use letters, digits, dot, underscore, colon or hyphen, up to 64 characters.` : null };
}

// One "key=value" per line.
export function parseKeyValues(text: string) {
  const values: Record<string, string> = {};
  for (const [i, raw] of text.split("\n").entries()) {
    const line = raw.trim();
    if (!line) continue;
    const at = line.indexOf("=");
    if (at < 1) return { values, error: `Line ${i + 1}: write it as key=value.` };
    values[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return { values, error: null };
}

export function formatKeyValues(values: Record<string, string>) {
  return Object.entries(values)
    .map(([key, value]) => `${key}=${value}`)
    .join("\n");
}

// Lines of JSON, one member each. Reports the first bad line instead of sending a broken batch.
export function parseMemberLines(text: string) {
  const members: MemberRequest[] = [];
  for (const [i, raw] of text.split("\n").entries()) {
    const line = raw.trim();
    if (!line) continue;
    try {
      const parsed = JSON.parse(line);
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("not an object");
      members.push(parsed as MemberRequest);
    } catch {
      return { members, error: `Line ${i + 1} is not a JSON object.` };
    }
  }
  if (members.length === 0) return { members, error: "Paste at least one JSON line." };
  if (members.length > 1000) return { members, error: "A single request takes at most 1000 members." };
  return { members, error: null };
}

export function isCode(error: unknown, code: string) {
  return isApiError(error) && error.code === code;
}

export function snapshotMessage(error: unknown) {
  if (!isApiError(error)) return describeError(error);
  switch (error.code) {
    case "no_members":
      return "The draft has no members, so there is nothing to snapshot. Add members first.";
    case "unresolved_members":
      return "Some members have no pinned version and their object has no current version. Pin a version or remove those members, then try again.";
    case "dataset_archived":
      return "This dataset is archived. Archived datasets are read-only, so no new snapshot can be made.";
    case "snapshot_too_large":
      return "The draft has more members than one snapshot can hold. Split it into smaller datasets.";
    default:
      return describeError(error);
  }
}

export function createMessage(error: unknown) {
  if (isCode(error, "idempotency_conflict")) {
    return "A dataset with this name already exists with different settings. Pick another name, or open the existing dataset to change it.";
  }
  return describeError(error);
}

export const conflictMessage =
  "Someone else changed this dataset while you were editing, so your edits were not saved.";

export function editMessage(error: unknown) {
  if (isCode(error, "revision_conflict")) return `${conflictMessage} Check the latest values and apply your change again.`;
  if (isCode(error, "dataset_archived")) return "This dataset is archived and cannot be changed.";
  return describeError(error);
}

export function shortHash(hash: string | undefined) {
  return hash ? hash.slice(0, 12) : "";
}

export function memberSourceLabel(source: string) {
  if (source === "api") return "Added by hand";
  if (source.startsWith("import:")) return `Import ${source.slice(7, 15)}`;
  if (source.startsWith("derived:")) return `Derived from version ${source.slice(8, 16)}`;
  return source;
}

export type PreflightResult = {
  lines: number;
  invalid: number;
  samples: { line: number; problem: string }[];
};

function checkLine(text: string): string | null {
  if (new TextEncoder().encode(text).length > MAX_LINE_BYTES) return "Line is longer than 64 KiB.";
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return "Not valid JSON.";
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return "Not a JSON object.";
  const line = parsed as Record<string, unknown>;
  if ("tenant_id" in line) return "tenant_id is not allowed.";
  if ("schema_version" in line && line.schema_version !== 1) return "schema_version must be 1.";
  const hasObject = line.object_id !== undefined;
  const hasPath = line.path !== undefined;
  if (hasObject === hasPath) return "Give exactly one of object_id or path.";
  if (line.version_id !== undefined && line.version !== undefined) return "Give version_id or version, not both.";
  return null;
}

// Reads the file in chunks and checks one line at a time, so a manifest of
// millions of lines never sits in memory. Only 50 problems are kept.
export async function preflightManifest(file: Blob, signal?: AbortSignal): Promise<PreflightResult> {
  const result: PreflightResult = { lines: 0, invalid: 0, samples: [] };
  const reader = file.stream().pipeThrough(new TextDecoderStream()).getReader();
  let lineNumber = 0;
  let rest = "";

  function handle(text: string) {
    lineNumber += 1;
    if (!text.trim()) return;
    result.lines += 1;
    const problem = checkLine(text);
    if (!problem) return;
    result.invalid += 1;
    if (result.samples.length < MAX_ERROR_SAMPLES) result.samples.push({ line: lineNumber, problem });
  }

  try {
    for (;;) {
      if (signal?.aborted) throw new DOMException("Aborted", "AbortError");
      const { value, done } = await reader.read();
      if (done) break;
      const parts = (rest + value).split("\n");
      rest = parts.pop() ?? "";
      parts.forEach(handle);
      // a single line with no break must not grow without limit
      if (rest.length > MAX_LINE_BYTES * 2) rest = rest.slice(0, MAX_LINE_BYTES * 2);
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  if (rest) handle(rest);
  return result;
}

export function importMessage(error: unknown) {
  if (!isApiError(error)) return describeError(error);
  switch (error.code) {
    case "too_large":
      return "The manifest is too large to send in one request. Use the staged upload instead.";
    case "manifest_not_uploaded":
      return "The storage service has not received the manifest file. Try again to upload it, then submit.";
    case "checksum_mismatch":
      return "The uploaded file does not match its SHA-256. Choose the file again and retry.";
    case "too_many_jobs":
      return error.retryAfter
        ? `This organization already has the maximum number of unfinished imports. Try again in ${error.retryAfter} seconds.`
        : "This organization already has the maximum number of unfinished imports. Wait for one to finish.";
    case "dataset_archived":
      return "This dataset is archived, so it cannot take imports.";
    case "idempotency_conflict":
      return "This attempt was already used with a different file. Choose the file again to start a new attempt.";
    default:
      return describeError(error);
  }
}

const RECENT_KEY = (datasetId: string) => `datasets.imports.${datasetId}`;

// The API cannot list imports, so the ids this browser started are remembered per dataset.
export function readRecentImports(datasetId: string): { id: string; at: string }[] {
  try {
    const raw = localStorage.getItem(RECENT_KEY(datasetId));
    return raw ? (JSON.parse(raw) as { id: string; at: string }[]) : [];
  } catch {
    return [];
  }
}

export function rememberImport(datasetId: string, id: string) {
  try {
    const next = [{ id, at: new Date().toISOString() }, ...readRecentImports(datasetId).filter((i) => i.id !== id)];
    localStorage.setItem(RECENT_KEY(datasetId), JSON.stringify(next.slice(0, 10)));
  } catch {
    // storage can be blocked; the job page is still reached from the redirect
  }
}
