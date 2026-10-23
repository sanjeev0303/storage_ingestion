"use client";

import Link from "next/link";
import { useRef, useState } from "react";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { describeError, isApiError } from "@/lib/api/errors";
import { newIdempotencyKey } from "@/lib/api/client";
import { useSession } from "@/lib/auth/session";
import { formatDateTime } from "@/lib/utils/format";
import { useProfiles, useStartJob, useVersions } from "../hooks";
import { buildStartRequest, MAX_SAMPLE_SIZE } from "../schema";
import { MODE_HELP, MODE_LABELS } from "../status";
import type { DatasetSummary, Mode } from "../types";
import type { StartResult } from "../api";

const MODES: Mode[] = ["METADATA", "FULL", "SAMPLED"];

export function startErrorMessage(error: unknown) {
  if (isApiError(error)) {
    if (error.code === "no_version") {
      return "This dataset has no snapshot yet, so there is nothing to check. Create a snapshot from the dataset's Versions tab, then start again.";
    }
    if (error.code === "dataset_archived")
      return "This dataset is archived. Archived datasets are read-only, so no new checks can start.";
    if (error.code === "too_many_jobs") {
      const wait = error.retryAfter
        ? ` Try again in about ${error.retryAfter} seconds.`
        : " Try again shortly.";
      return `Too many validations are already running for this organization.${wait}`;
    }
  }
  return describeError(error);
}

type Props = {
  dataset: DatasetSummary;
  kind: "validation" | "readiness";
  initialVersionId?: string;
};

export function StartForm({ dataset, kind, initialVersionId }: Props) {
  const { can } = useSession();
  const readiness = kind === "readiness";
  const canStart = can("datasets.write");

  const versions = useVersions(dataset.id);
  const profiles = useProfiles();
  const start = useStartJob(dataset.id, kind);

  const [mode, setMode] = useState<Mode>("METADATA");
  const [sampleSize, setSampleSize] = useState("");
  const [versionId, setVersionId] = useState(initialVersionId ?? "");
  const [profileId, setProfileId] = useState("");
  const [result, setResult] = useState<StartResult | null>(null);
  // One key per attempt: a retry after an error reuses it, a changed form starts a new attempt.
  const keyRef = useRef(newIdempotencyKey());
  const newAttempt = () => {
    keyRef.current = newIdempotencyKey();
    setResult(null);
  };

  const profileList = profiles.data?.profiles ?? [];
  const effectiveVersionId = versionId || dataset.current_version_id || "";
  const effectiveProfileId =
    profileId ||
    dataset.profile_id ||
    (profileList.some((p) => p.id === "builtin:general")
      ? "builtin:general"
      : profileList.length === 1
        ? profileList[0].id
        : "");
  const profile = profileList.find((p) => p.id === effectiveProfileId);
  const versionList = versions.items;
  const idPrefix = kind;

  const built = buildStartRequest(
    {
      mode,
      sampleSize,
      versionId: effectiveVersionId,
      profileId: effectiveProfileId,
      profileVersion: profile?.version,
    },
    "",
    { profileRequired: readiness },
  );
  const errors = built.errors;
  const valid =
    canStart && Boolean(effectiveVersionId) && Boolean(built.request);
  const hint = !canStart
    ? "You do not have permission to start checks."
    : !effectiveVersionId
      ? "This dataset needs a snapshot before it can be checked."
      : errors.profile
        ? "Choose a profile to continue."
        : "Enter a sample size from 1 to 10,000, or leave it empty.";

  function submit(event: React.FormEvent) {
    event.preventDefault();
    if (!valid || !built.request) return;
    start.mutate(
      { ...built.request, idempotency_key: keyRef.current },
      {
        onSuccess: (data) => {
          setResult(data);
          keyRef.current = newIdempotencyKey();
        },
      },
    );
  }

  return (
    <Panel
      title={readiness ? "Assess readiness" : "Validate a version"}
      description={
        readiness
          ? "Judges the version against a profile across nine dimensions. There is no overall score."
          : "Checks one snapshot against a profile and writes a report."
      }
    >
      <form onSubmit={submit} noValidate className="flex flex-col gap-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Dataset version"
            required
            hint={
              dataset.current_version_id
                ? "Defaults to the current version."
                : "No snapshot exists yet."
            }
          >
            {(props) => (
              <Select
                {...props}
                value={effectiveVersionId}
                onChange={(e) => {
                  setVersionId(e.target.value);
                  newAttempt();
                }}
              >
                {!effectiveVersionId && (
                  <option value="">No snapshot yet</option>
                )}
                {effectiveVersionId &&
                  !versionList.some((v) => v.id === effectiveVersionId) && (
                    <option value={effectiveVersionId}>
                      {versions.isPending
                        ? "Loading versions"
                        : `Version ${effectiveVersionId.slice(0, 8)}`}
                    </option>
                  )}
                {versionList.map((v) => (
                  <option key={v.id} value={v.id}>
                    {`Version ${v.version}${v.id === dataset.current_version_id ? " (current)" : ""}, ${v.member_count} members, ${formatDateTime(v.created_at)}`}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field
            label="Profile"
            required={readiness}
            error={errors.profile}
            hint={
              readiness
                ? "Required. The assessment is against the profile you pick."
                : dataset.profile_id
                  ? "Defaults to the dataset's profile."
                  : "Defaults to the built-in general profile."
            }
          >
            {(props) => (
              <Select
                {...props}
                value={effectiveProfileId}
                onChange={(e) => {
                  setProfileId(e.target.value);
                  newAttempt();
                }}
              >
                {!effectiveProfileId && (
                  <option value="">Choose a profile</option>
                )}
                {effectiveProfileId && !profile && (
                  <option value={effectiveProfileId}>
                    {effectiveProfileId}
                  </option>
                )}
                {profileList.map((p) => (
                  <option key={p.id} value={p.id}>
                    {`${p.name} (v${p.version}${p.built_in ? ", built in" : ""})`}
                  </option>
                ))}
              </Select>
            )}
          </Field>
        </div>

        <fieldset className="flex flex-col gap-3">
          <legend className="mb-1 text-sm font-medium">What to examine</legend>
          {MODES.map((m) => (
            <label key={m} className="flex cursor-pointer items-start gap-3">
              <input
                type="radio"
                name={`${idPrefix}-mode`}
                value={m}
                checked={mode === m}
                onChange={() => {
                  setMode(m);
                  newAttempt();
                }}
                className="mt-1 size-4 shrink-0 accent-(--ink)"
              />
              <span>
                <span className="block text-base font-medium">
                  {MODE_LABELS[m]}
                </span>
                <span className="block max-w-prose text-sm text-muted">
                  {MODE_HELP[m]}
                </span>
              </span>
            </label>
          ))}
        </fieldset>

        {mode === "SAMPLED" && (
          <Field
            label="Sample size"
            error={errors.sampleSize}
            hint={`Members whose content is checked, 1 to ${MAX_SAMPLE_SIZE.toLocaleString()}. Leave empty for the server default (100).`}
            className="max-w-xs"
          >
            {(props) => (
              <Input
                {...props}
                inputMode="numeric"
                value={sampleSize}
                onChange={(e) => {
                  setSampleSize(e.target.value);
                  newAttempt();
                }}
              />
            )}
          </Field>
        )}

        <div className="flex flex-wrap items-center gap-3">
          <SubmitButton valid={valid} loading={start.isPending} hint={hint}>
            {readiness ? "Start assessment" : "Start validation"}
          </SubmitButton>
          {!canStart && (
            <p className="text-sm text-muted">
              Your role cannot start checks. Starting needs the datasets.write
              permission.
            </p>
          )}
        </div>

        {start.isError && (
          <p role="alert" className="max-w-prose text-base font-medium">
            {startErrorMessage(start.error)}
          </p>
        )}
        {result && (
          <p role="status" className="max-w-prose text-base">
            {result.created
              ? "The backend accepted the job; it is queued. "
              : "The backend already had a job for this request, so this is that earlier job. "}
            <Link
              href={`/app/validation/jobs/${result.job.id}`}
              className="font-semibold underline underline-offset-2"
            >
              Open the job
            </Link>
          </p>
        )}
      </form>
    </Panel>
  );
}
