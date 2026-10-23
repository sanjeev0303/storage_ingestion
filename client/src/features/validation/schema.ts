import { cleanText, cleanWholeNumber } from "@/lib/utils/forgive";
import type { CreateProfileRequest, Mode, Scope, Severity, ValidationRequest } from "./types";

export const MAX_SAMPLE_SIZE = 10000;

export type StartForm = {
  mode: Mode;
  sampleSize: string;
  versionId: string;
  profileId: string;
  profileVersion: number | undefined;
};

export type StartErrors = { sampleSize?: string; profile?: string; version?: string };

// sample_size is only allowed with SAMPLED; the API answers 400 for any other mode.
export function buildStartRequest(
  form: StartForm,
  idempotencyKey: string,
  options: { profileRequired: boolean },
): { request?: ValidationRequest; errors: StartErrors } {
  const errors: StartErrors = {};
  const request: ValidationRequest = { mode: form.mode, idempotency_key: idempotencyKey };

  if (form.mode === "SAMPLED" && form.sampleSize.trim() !== "") {
    const size = Number(cleanWholeNumber(form.sampleSize));
    if (size < 1 || size > MAX_SAMPLE_SIZE) {
      errors.sampleSize = `Use a whole number from 1 to ${MAX_SAMPLE_SIZE.toLocaleString()}.`;
    } else {
      request.sample_size = size;
    }
  }

  if (form.versionId) request.version_id = form.versionId;
  if (form.profileId) {
    request.profile_id = form.profileId;
    if (form.profileVersion) request.profile_version = form.profileVersion;
  } else if (options.profileRequired) {
    errors.profile = "A readiness assessment judges the dataset against a profile. Choose one.";
  }

  if (Object.keys(errors).length > 0) return { errors };
  return { request, errors };
}

export type RuleDraft = {
  id: string;
  version: string;
  type: string;
  severity: Severity;
  enabled: boolean;
  scope: Scope | "";
  config: string;
};

export type ProfileDraft = {
  name: string;
  description: string;
  rules: RuleDraft[];
  maxErrors: string;
  maxWarnings: string;
};

export type ProfileErrors = {
  name?: string;
  description?: string;
  rules?: string;
  maxErrors?: string;
  maxWarnings?: string;
  rule: Record<number, { id?: string; version?: string; type?: string; config?: string }>;
};

export function parseConfig(text: string): { value?: Record<string, unknown>; error?: string } {
  // A pasted file can start with a byte order mark.
  const clean = text.replace(/^﻿/, "");
  if (clean.trim() === "") return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(clean);
  } catch (e) {
    const reason = e instanceof Error ? ` ${e.message}.` : "";
    return { error: `This is not valid JSON.${reason}` };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { error: "Config must be a JSON object, like {\"keys\": [\"license\"]}." };
  }
  return { value: parsed as Record<string, unknown> };
}

function wholeNumber(text: string, min: number) {
  const digits = cleanWholeNumber(text);
  const n = Number(digits);
  return digits !== "" && n >= min ? n : null;
}

export function buildProfileRequest(
  draft: ProfileDraft,
  ruleTypes: string[],
): { request?: CreateProfileRequest; errors: ProfileErrors } {
  const errors: ProfileErrors = { rule: {} };
  const name = cleanText(draft.name);
  if (name.length < 1 || name.length > 128) errors.name = "Name is required, up to 128 characters.";
  if (draft.description.length > 2000) errors.description = "Description can be up to 2000 characters.";

  if (draft.rules.length === 0) errors.rules = "Add at least one rule.";
  if (draft.rules.length > 100) errors.rules = "A profile can have up to 100 rules.";

  const seen = new Set<string>();
  const rules: CreateProfileRequest["rules"] = [];
  draft.rules.forEach((rule, index) => {
    const e: ProfileErrors["rule"][number] = {};
    const id = cleanText(rule.id);
    if (!id) e.id = "Each rule needs an id.";
    else if (seen.has(id)) e.id = "Rule ids must be unique within the profile.";
    seen.add(id);

    const version = wholeNumber(rule.version, 1);
    if (version === null) e.version = "Version is a whole number, 1 or more.";
    if (!ruleTypes.includes(rule.type)) e.type = "Choose a rule type.";

    const config = parseConfig(rule.config);
    if (config.error) e.config = config.error;

    if (Object.keys(e).length > 0) {
      errors.rule[index] = e;
      return;
    }
    rules.push({
      id,
      version: version as number,
      type: rule.type,
      severity: rule.severity,
      enabled: rule.enabled,
      ...(rule.scope ? { scope: rule.scope } : {}),
      ...(config.value ? { config: config.value } : {}),
    });
  });

  const maxErrors = wholeNumber(draft.maxErrors, 0);
  if (maxErrors === null) errors.maxErrors = "Use a whole number, 0 or more.";
  let maxWarnings: number | null = null;
  if (draft.maxWarnings.trim() !== "") {
    maxWarnings = wholeNumber(draft.maxWarnings, 0);
    if (maxWarnings === null) errors.maxWarnings = "Use a whole number, 0 or more, or leave it empty.";
  }

  const hasErrors = Object.keys(errors).length > 1 || Object.keys(errors.rule).length > 0;
  if (hasErrors) return { errors };

  return {
    errors,
    request: {
      name,
      ...(draft.description.trim() ? { description: draft.description.trim() } : {}),
      rules,
      thresholds: { max_errors: maxErrors as number, ...(maxWarnings !== null ? { max_warnings: maxWarnings } : {}) },
    },
  };
}
