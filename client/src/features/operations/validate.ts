import type { RetentionScope } from "./types";

export const MAX_REASON = 500;

// Go durations: a number then ns, us, ms, s, m or h, repeated. No d or w.
const GO_DURATION = /^(\d+(\.\d+)?(ns|us|ms|s|m|h))+$/;
const DATASET_NAME = /^[a-z0-9][a-z0-9._-]{0,63}$/;

// Returns a message, or null when the value is empty (inherit) or valid.
export function validateDuration(value: string) {
  const text = value.trim();
  if (text === "") return null;
  if (/\d\s*[dw]\b/i.test(text) || /\d\s*(days?|weeks?)/i.test(text)) {
    return "Days and weeks are not accepted. Use hours: 7 days is 168h, 30 days is 720h.";
  }
  if (!GO_DURATION.test(text)) {
    return "Write it like 720h or 1h30m. Units are ns, us, ms, s, m and h.";
  }
  return null;
}

export function validateScopeKey(scope: RetentionScope, key: string) {
  const value = key.trim();
  if (scope === "TENANT") {
    return value === "" ? null : "The whole-organization scope takes no key. Leave this empty.";
  }
  if (value === "") {
    return scope === "DATASET" ? "Enter the dataset name." : "Enter the asset id.";
  }
  if (scope === "DATASET" && !DATASET_NAME.test(value)) {
    return "Dataset names are 1 to 64 characters: lowercase letters, digits, dots, underscores and hyphens, starting with a letter or digit.";
  }
  if (scope === "ASSET" && value.length > 200) return "Asset ids are at most 200 characters.";
  return null;
}

export function validateReason(value: string) {
  const text = value.trim();
  if (text === "") return "A reason is required. It is written to the audit log.";
  if (new TextEncoder().encode(text).length > MAX_REASON) return `Keep the reason to ${MAX_REASON} characters or fewer.`;
  return null;
}
