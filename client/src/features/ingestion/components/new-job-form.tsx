"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useRef, useState, type FormEvent } from "react";
import { Button, ButtonLink } from "@/components/ui/button";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { describeError, isApiError } from "@/lib/api/errors";
import { newIdempotencyKey } from "@/lib/api/client";
import { useQueryClient } from "@tanstack/react-query";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { cleanWholeNumber } from "@/lib/utils/forgive";
import { createJob } from "../api";
import { useDatasets, useManifestLimits } from "../hooks";
import type { IngestionJob } from "../types";

export function messageForCreateError(error: unknown) {
  if (isApiError(error)) {
    if (error.status === 409) return "That attempt's key was already used for a different job. Try again to get a fresh key.";
    if (error.status === 403) return `${error.message} Your organization may be at its limit of active jobs. Finish or cancel one, then try again.`;
    if (error.status === 404) return "That dataset was not found. Pick another one.";
  }
  return describeError(error);
}

export function NewJobForm() {
  const router = useRouter();
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const datasets = useDatasets();
  const quota = useManifestLimits();

  const [datasetId, setDatasetId] = useState("");
  const [concurrency, setConcurrency] = useState("0");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [existing, setExisting] = useState<IngestionJob | null>(null);
  // One key per attempt: a retry of the same inputs reuses it, changed inputs get a new one.
  const attempt = useRef({ signature: "", key: "" });

  // A lone dataset is picked for the person.
  const effectiveDatasetId = datasetId || (datasets.items.length === 1 ? datasets.items[0].id : "");
  const datasetError = effectiveDatasetId ? undefined : "Choose a dataset.";

  const cleanedConcurrency = cleanWholeNumber(concurrency);
  const parsedConcurrency = Number(cleanedConcurrency);
  const concurrencyError =
    cleanedConcurrency === "" || parsedConcurrency > 32
      ? "Enter a whole number from 0 to 32. Use 0 for the server default."
      : undefined;
  const valid = !datasetError && !concurrencyError;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    const signature = `${effectiveDatasetId}:${parsedConcurrency}`;
    if (attempt.current.signature !== signature) attempt.current = { signature, key: newIdempotencyKey() };

    setBusy(true);
    setError(null);
    setExisting(null);
    try {
      const { job, existed } = await createJob({
        dataset_id: effectiveDatasetId,
        idempotency_key: attempt.current.key,
        max_concurrency: parsedConcurrency,
      });
      client.invalidateQueries({ queryKey: orgKey(organizationId, "ingestion") });
      if (existed) setExisting(job);
      else router.push(`/app/ingestion/jobs/${job.id}`);
    } catch (e) {
      if (isApiError(e) && e.status === 409) attempt.current = { signature: "", key: "" };
      setError(messageForCreateError(e));
    } finally {
      setBusy(false);
    }
  }

  if (!can("ingestion.write")) {
    return <PermissionDenied detail="Your role cannot create ingestion jobs. It needs ingestion.write." />;
  }
  if (datasets.isPending) return <SkeletonRows rows={3} />;
  if (datasets.isError) return <ErrorState error={datasets.error} onRetry={() => datasets.refetch()} />;
  if (datasets.items.length === 0) {
    return (
      <EmptyState
        title="Create a dataset first"
        action={
          <ButtonLink href="/app/datasets/new" variant="primary">
            Create a dataset
          </ButtonLink>
        }
      >
        An ingestion job loads files into an existing dataset. This organization has none yet.
      </EmptyState>
    );
  }

  const maxActive = quota.data?.limits.max_active_jobs;

  return (
    <Panel title="New ingestion job" description="The job holds one manifest. You submit the manifest on the next screen.">
      <form onSubmit={submit} className="flex max-w-xl flex-col gap-5" noValidate>
        <Field label="Dataset" required error={datasetError} hint="The files in the manifest are added to this dataset.">
          {(props) => (
            <Select {...props} value={effectiveDatasetId} onChange={(e) => setDatasetId(e.target.value)} required>
              <option value="">Choose a dataset</option>
              {datasets.items.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name}
                </option>
              ))}
            </Select>
          )}
        </Field>
        {datasets.hasNextPage && (
          <Button onClick={() => datasets.fetchNextPage()} loading={datasets.isFetchingNextPage} className="self-start">
            Load more datasets
          </Button>
        )}
        <p className="text-sm text-muted">
          Not listed? <Link href="/app/datasets/new" className="underline underline-offset-2">Create a dataset</Link>.
        </p>

        <Field
          label="Upload concurrency"
          required
          error={concurrencyError}
          hint="How many files this job may upload at once, 0 to 32. 0 uses the server default. This browser never runs more than 6."
        >
          {(props) => (
            <Input {...props} type="number" inputMode="numeric" min={0} max={32} value={concurrency} onChange={(e) => setConcurrency(e.target.value)} />
          )}
        </Field>

        {maxActive ? (
          <p className="text-sm text-muted">Your organization can have {maxActive} active ingestion jobs at once.</p>
        ) : null}

        {existing && (
          <div role="status" className="neo-in-sm flex flex-col items-start gap-2 p-4">
            <p className="font-medium">This job already existed.</p>
            <p className="text-sm text-muted">The same attempt was already sent, so no second job was made.</p>
            <ButtonLink href={`/app/ingestion/jobs/${existing.id}`} variant="primary">
              Open the job
            </ButtonLink>
          </div>
        )}
        {error && (
          <p role="alert" className="font-medium">
            {error}
          </p>
        )}

        <div className="flex gap-2">
          <SubmitButton
            valid={valid}
            loading={busy}
            hint={datasetError ? "Choose a dataset to continue." : "Enter a concurrency from 0 to 32 to continue."}
          >
            Create job
          </SubmitButton>
          <ButtonLink href="/app/ingestion/jobs">Cancel</ButtonLink>
        </div>
      </form>
    </Panel>
  );
}
