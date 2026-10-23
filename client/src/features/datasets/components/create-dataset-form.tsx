"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState, type FormEvent } from "react";
import { ButtonLink } from "@/components/ui/button";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/field";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { PermissionDenied } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { useSession } from "@/lib/auth/session";
import { cleanWholeNumber, splitList } from "@/lib/utils/forgive";
import { createMessage, parseKeyValues, parseTags, SPLITS, validateName } from "../helpers";
import { useCreateDataset, useValidationProfiles } from "../hooks";
import type { CompletionPolicy, CreateDatasetRequest, Dataset, Split } from "../types";

const policies: { value: CompletionPolicy; label: string }[] = [
  { value: "ALL", label: "ALL: every entry in a bulk ingestion must succeed" },
  { value: "REQUIRED", label: "REQUIRED: entries marked required must succeed" },
  { value: "PARTIAL", label: "PARTIAL: usable with whatever succeeded" },
];

type Errors = Partial<Record<"name" | "description" | "tags" | "metadata" | "maxBytes" | "derived", string>>;

export function CreateDatasetForm() {
  const { can } = useSession();
  const router = useRouter();
  const create = useCreateDataset();
  const profiles = useValidationProfiles();

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [tagText, setTagText] = useState("");
  const [metaText, setMetaText] = useState("");
  const [policy, setPolicy] = useState<CompletionPolicy>("ALL");
  const [maxBytes, setMaxBytes] = useState("");
  const [profile, setProfile] = useState("");
  const [versionIds, setVersionIds] = useState("");
  const [splits, setSplits] = useState<Split[]>([]);
  const [labels, setLabels] = useState("");
  const [transformName, setTransformName] = useState("");
  const [transformVersion, setTransformVersion] = useState("");
  const [existing, setExisting] = useState<Dataset | null>(null);

  if (!can("datasets.write")) {
    return (
      <>
        <PageHeader title="New dataset" crumbs={[{ label: "Datasets", href: "/app/datasets" }, { label: "New" }]} />
        <PermissionDenied detail="Creating a dataset needs the datasets.write permission." />
      </>
    );
  }

  const nameProblem = validateName(name.trim());
  const tags = parseTags(tagText);
  const meta = parseKeyValues(metaText);
  const bytes = cleanWholeNumber(maxBytes);
  const ids = splitList(versionIds);
  const errors: Errors = {
    name: nameProblem ?? undefined,
    description: description.length > 2000 ? "Keep the description to 2000 characters." : undefined,
    tags: tags.error ?? undefined,
    metadata: meta.error ?? undefined,
    maxBytes: maxBytes.trim() && !bytes ? "Enter a whole number of bytes, or leave it empty." : undefined,
    derived:
      ids.length > 16
        ? "A derived dataset takes at most 16 source versions."
        : transformName.trim() && ids.length === 0
          ? "Add the source version ids this dataset derives from."
          : undefined,
  };
  const valid = Object.values(errors).every((error) => !error);
  const missing = nameProblem ? "Enter a valid name to continue." : "Fix the highlighted fields to continue.";

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;

    const body: CreateDatasetRequest = { name: name.trim(), completion_policy: policy };
    if (description.trim()) body.description = description.trim();
    if (tags.tags.length) body.tags = tags.tags;
    if (Object.keys(meta.values).length) body.metadata = meta.values;
    if (bytes) body.max_bytes = Number(bytes);
    if (profile) {
      const [id, version] = profile.split("@");
      body.profile_id = id;
      body.profile_version = Number(version);
    }
    if (ids.length) {
      body.derived_from = { version_ids: ids };
      if (splits.length) body.derived_from.splits = splits;
      // A label may contain spaces, so only commas and new lines separate them.
      const labelList = labels
        .split(/[,\n]/)
        .map((label) => label.trim())
        .filter(Boolean);
      if (labelList.length) body.derived_from.labels = labelList;
      if (transformName.trim()) {
        body.derived_from.transformation = { name: transformName.trim(), version: transformVersion.trim() || undefined };
      }
    }

    setExisting(null);
    create.mutate(body, {
      onSuccess: ({ dataset, created }) => {
        if (created) router.push(`/app/datasets/${dataset.id}`);
        else setExisting(dataset);
      },
    });
  }

  function toggleSplit(split: Split) {
    setSplits((current) => (current.includes(split) ? current.filter((s) => s !== split) : [...current, split]));
  }

  return (
    <>
      <PageHeader
        title="New dataset"
        description="This creates an empty, mutable draft. Add members, then take a snapshot to get an immutable version."
        crumbs={[{ label: "Datasets", href: "/app/datasets" }, { label: "New" }]}
      />

      {existing && (
        <div role="status" className="neo-in mb-6 flex flex-col items-start gap-3 p-6">
          <h2 className="text-md font-semibold">This dataset already existed</h2>
          <p className="max-w-prose text-base text-muted">
            A dataset named {existing.name} with the same settings was already there, so nothing new was created.
          </p>
          <ButtonLink href={`/app/datasets/${existing.id}`} variant="primary">
            Open {existing.name}
          </ButtonLink>
        </div>
      )}

      <form onSubmit={submit} noValidate className="grid max-w-3xl gap-6" aria-label="New dataset">
        <Panel title="Basics">
          <div className="grid gap-4">
            <Field
              label="Name"
              required
              error={errors.name}
              hint="Lowercase letters, digits, dot, underscore and hyphen, up to 64. The name is also the catalog namespace label."
            >
              {(props) => (
                <Input
                  {...props}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  autoComplete="off"
                  spellCheck={false}
                  maxLength={64}
                />
              )}
            </Field>
            <Field label="Description" error={errors.description} hint="Up to 2000 characters.">
              {(props) => (
                <Textarea {...props} value={description} onChange={(e) => setDescription(e.target.value)} maxLength={2000} />
              )}
            </Field>
            <Field label="Tags" error={errors.tags} hint="Separate with commas. Up to 64 characters each.">
              {(props) => (
                <Input {...props} value={tagText} onChange={(e) => setTagText(e.target.value)} autoComplete="off" />
              )}
            </Field>
            <Field label="Metadata" error={errors.metadata} hint="One key=value per line.">
              {(props) => (
                <Textarea
                  {...props}
                  value={metaText}
                  onChange={(e) => setMetaText(e.target.value)}
                  spellCheck={false}
                  className="mono-data"
                />
              )}
            </Field>
          </div>
        </Panel>

        <Panel
          title="Bulk ingestion and validation"
          description="Completion policy and byte limit only govern bulk ingestion into this dataset. They do not affect snapshots."
        >
          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              label="Completion policy"
              hint="Decides when a bulk ingestion counts as complete: every entry, only the required ones, or whatever arrived."
            >
              {(props) => (
                <Select {...props} value={policy} onChange={(e) => setPolicy(e.target.value as CompletionPolicy)}>
                  {policies.map((p) => (
                    <option key={p.value} value={p.value}>
                      {p.label}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field label="Byte limit" error={errors.maxBytes} hint="In bytes. Leave empty or 0 to apply only the organization quota.">
              {(props) => (
                <Input
                  {...props}
                  inputMode="numeric"
                  value={maxBytes}
                  onChange={(e) => setMaxBytes(e.target.value)}
                  autoComplete="off"
                />
              )}
            </Field>
            <Field
              label="Validation profile"
              className="sm:col-span-2"
              hint={
                profiles.isError
                  ? "Profiles could not be loaded. You can choose one later; validation falls back to the general profile."
                  : "The rules this dataset is checked against by default. Optional."
              }
            >
              {(props) => (
                <Select {...props} value={profile} onChange={(e) => setProfile(e.target.value)}>
                  <option value="">No default profile</option>
                  {profiles.data?.profiles.map((p) => (
                    <option key={`${p.id}@${p.version}`} value={`${p.id}@${p.version}`}>
                      {p.name} (version {p.version})
                    </option>
                  ))}
                </Select>
              )}
            </Field>
          </div>
        </Panel>

        <details className="neo-in-sm p-5">
          <summary className="cursor-pointer text-md font-semibold">Advanced: derive from existing versions</summary>
          <p className="mt-2 max-w-prose text-base text-muted">
            Start the draft from the members of up to 16 snapshots. Every member is pinned to the exact object
            version the source held. Version ids are the version UUIDs shown on the Versions tab.
          </p>
          <div className="mt-4 grid gap-4">
            <Field label="Source version ids" error={errors.derived} hint="One per line or separated by commas, up to 16.">
              {(props) => (
                <Textarea
                  {...props}
                  value={versionIds}
                  onChange={(e) => setVersionIds(e.target.value)}
                  spellCheck={false}
                  className="mono-data"
                />
              )}
            </Field>
            <fieldset>
              <legend className="mb-1.5 text-sm font-medium">Only these splits</legend>
              <div className="flex flex-wrap gap-4">
                {SPLITS.map((split) => (
                  <Checkbox key={split} label={split} checked={splits.includes(split)} onChange={() => toggleSplit(split)} />
                ))}
              </div>
            </fieldset>
            <Field label="Only these labels" hint="Separate with commas or new lines.">
              {(props) => <Input {...props} value={labels} onChange={(e) => setLabels(e.target.value)} />}
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Transformation name" hint="Recorded in lineage.">
                {(props) => <Input {...props} value={transformName} onChange={(e) => setTransformName(e.target.value)} />}
              </Field>
              <Field label="Transformation version">
                {(props) => (
                  <Input {...props} value={transformVersion} onChange={(e) => setTransformVersion(e.target.value)} />
                )}
              </Field>
            </div>
          </div>
        </details>

        {create.isError && (
          <p role="alert" className="max-w-prose text-base font-medium">
            {createMessage(create.error)}
          </p>
        )}

        <div className="flex flex-wrap gap-2">
          <SubmitButton valid={valid} loading={create.isPending} hint={missing}>
            Create dataset
          </SubmitButton>
          <Link href="/app/datasets" className="inline-flex h-9 items-center px-4 text-base font-medium hover:underline">
            Cancel
          </Link>
        </div>
      </form>
    </>
  );
}
