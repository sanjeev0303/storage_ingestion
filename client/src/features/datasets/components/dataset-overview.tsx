"use client";

import Link from "next/link";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { CopyButton } from "@/components/ui/copy-button";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { ApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { conflictMessage, editMessage, formatKeyValues, isCode, parseKeyValues, parseTags } from "../helpers";
import { useArchiveDataset, useDataset, useUpdateDataset, useValidationProfiles } from "../hooks";
import type { Dataset, UpdateDatasetRequest } from "../types";
import { DraftVsSnapshot } from "./draft-notice";
import { SnapshotControl } from "./snapshot-control";

export function DatasetOverview({ id }: { id: string }) {
  const dataset = useDataset(id);
  if (dataset.isPending) return <SkeletonRows rows={5} />;
  if (dataset.isError) return <ErrorState error={dataset.error} onRetry={() => dataset.refetch()} />;
  return <OverviewBody dataset={dataset.data} />;
}

function OverviewBody({ dataset }: { dataset: Dataset }) {
  const { can } = useSession();
  const [editing, setEditing] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const archive = useArchiveDataset(dataset.id);
  const archived = dataset.lifecycle_status === "ARCHIVED";
  const metadata = Object.entries(dataset.metadata);

  async function confirmArchive() {
    try {
      await archive.mutateAsync();
    } catch (error) {
      if (isCode(error, "invalid_transition")) {
        throw new ApiError({
          status: 409,
          code: "invalid_transition",
          message: "A validation is running on this dataset. Wait for it to finish or cancel it, then archive.",
        });
      }
      throw error;
    }
  }

  return (
    <div className="grid gap-6">
      <DraftVsSnapshot />

      <div className="grid gap-6 lg:grid-cols-3">
        <Panel
          className="lg:col-span-2"
          title="Dataset"
          actions={
            <>
              {can("datasets.write") && !archived && <Button onClick={() => setEditing(true)}>Edit</Button>}
              <SnapshotControl dataset={dataset} />
            </>
          }
        >
          <DefList
            items={[
              { label: "Name", value: dataset.name },
              { label: "Members in the draft", value: formatNumber(dataset.member_count) },
              { label: "Revision", value: <span className="tabular-nums">{dataset.revision}</span> },
              { label: "Description", value: dataset.description || <span className="text-muted">None</span> },
              {
                label: "Tags",
                value: dataset.tags.length ? dataset.tags.join(", ") : <span className="text-muted">None</span>,
              },
              {
                label: "Current version",
                value: dataset.current_version_id ? (
                  <Link
                    href={`/app/datasets/${dataset.id}/versions/${dataset.current_version_id}`}
                    className="mono-data underline underline-offset-2"
                  >
                    {dataset.current_version_id}
                  </Link>
                ) : (
                  <span className="text-muted">No snapshot yet</span>
                ),
              },
              {
                label: "Latest validation report",
                value: dataset.latest_report_id ? (
                  <Link
                    href={`/app/validation/reports/${dataset.latest_report_id}`}
                    className="mono-data underline underline-offset-2"
                  >
                    {dataset.latest_report_id}
                  </Link>
                ) : (
                  <span className="text-muted">Not validated</span>
                ),
              },
              {
                label: "Default profile",
                value: dataset.profile_id ? (
                  <span className="mono-data">
                    {dataset.profile_id}
                    {dataset.profile_version ? `@${dataset.profile_version}` : ""}
                  </span>
                ) : (
                  <span className="text-muted">None</span>
                ),
              },
              {
                label: "Storage connection",
                value: dataset.storage_connection_id ? (
                  <Link href={`/app/storage/${dataset.storage_connection_id}`} className="mono-data underline underline-offset-2">
                    {dataset.storage_connection_id}
                  </Link>
                ) : (
                  <span className="text-muted">Organization default</span>
                ),
              },
              { label: "Created", value: <Time value={dataset.created_at} /> },
              { label: "Updated", value: <Time value={dataset.updated_at} /> },
              ...(dataset.archived_at ? [{ label: "Archived", value: <Time value={dataset.archived_at} /> }] : []),
              { label: "Id", value: <span className="mono-data">{dataset.id}</span> },
            ]}
          />
          {metadata.length > 0 && (
            <div className="mt-5">
              <h3 className="text-sm text-muted">Metadata</h3>
              <ul className="mt-1 grid gap-1 sm:grid-cols-2">
                {metadata.map(([key, value]) => (
                  <li key={key} className="mono-data">
                    {key} = {value}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </Panel>

        <div className="grid content-start gap-6">
          <Panel title="Bulk ingestion" description="Settings that apply to bulk ingestion, not to snapshots.">
            <DefList
              items={[
                { label: "Completion policy", value: dataset.completion_policy },
                { label: "Byte limit", value: dataset.max_bytes ? formatBytes(dataset.max_bytes) : "None" },
                { label: "Stored", value: formatBytes(dataset.stored_bytes) },
                { label: "Ingestion status", value: dataset.status },
              ]}
            />
          </Panel>

          {dataset.derived_from && (
            <Panel title="Derived from" description="This draft started from these snapshots.">
              <ul className="grid gap-2">
                {dataset.derived_from.version_ids.map((versionId) => (
                  <li key={versionId} className="flex items-center justify-between gap-2">
                    <span className="mono-data">{versionId}</span>
                    <CopyButton value={versionId} />
                  </li>
                ))}
              </ul>
              {dataset.derived_from.splits?.length ? (
                <p className="mt-3 text-sm text-muted">Splits: {dataset.derived_from.splits.join(", ")}</p>
              ) : null}
              {dataset.derived_from.labels?.length ? (
                <p className="mt-1 text-sm text-muted">Labels: {dataset.derived_from.labels.join(", ")}</p>
              ) : null}
              {dataset.derived_from.transformation && (
                <p className="mt-1 text-sm text-muted">
                  Transformation: {dataset.derived_from.transformation.name} {dataset.derived_from.transformation.version}
                </p>
              )}
            </Panel>
          )}

          {can("datasets.manage") && !archived && (
            <Panel title="Archive" description="Archiving is final. The dataset becomes read-only and cannot be reopened.">
              <p className="mb-3 text-base text-muted">
                Versions, reports and lineage stay readable. Waiting imports are cancelled and running ones are asked to
                stop.
              </p>
              <Button variant="accent" onClick={() => setArchiving(true)}>
                Archive dataset
              </Button>
            </Panel>
          )}
        </div>
      </div>

      {editing && <EditDialog dataset={dataset} onClose={() => setEditing(false)} />}

      <ConfirmDialog
        open={archiving}
        onOpenChange={setArchiving}
        title={`Archive ${dataset.name}`}
        description="The dataset becomes read-only for good. Nobody can add members, snapshot or validate it again. Existing versions, reports and lineage remain."
        confirmLabel="Archive dataset"
        confirmText={dataset.name}
        onConfirm={confirmArchive}
      />
    </div>
  );
}

function EditDialog({ dataset, onClose }: { dataset: Dataset; onClose: () => void }) {
  const update = useUpdateDataset(dataset.id);
  // The form is keyed by revision: after a conflict the latest values load and replace the stale form.
  const [conflict, setConflict] = useState(false);

  return (
    <Modal open onOpenChange={(open) => !open && !update.isPending && onClose()} title="Edit dataset">
      {conflict && (
        <p role="alert" className="mb-4 text-base font-medium">
          {conflictMessage} The latest values are now in the form. Apply your change again.
        </p>
      )}
      <EditForm
        key={dataset.revision}
        dataset={dataset}
        pending={update.isPending}
        error={update.isError && !conflict ? update.error : null}
        onCancel={onClose}
        onSave={(body) => {
          setConflict(false);
          update.mutate(body, {
            onSuccess: onClose,
            onError: (error) => setConflict(isCode(error, "revision_conflict")),
          });
        }}
      />
    </Modal>
  );
}

function EditForm({
  dataset,
  pending,
  error,
  onSave,
  onCancel,
}: {
  dataset: Dataset;
  pending: boolean;
  error: unknown;
  onSave: (body: UpdateDatasetRequest) => void;
  onCancel: () => void;
}) {
  const { can } = useSession();
  const profiles = useValidationProfiles();
  const [description, setDescription] = useState(dataset.description);
  const [tagText, setTagText] = useState(dataset.tags.join(", "));
  const [metaText, setMetaText] = useState(formatKeyValues(dataset.metadata));
  const [profile, setProfile] = useState(dataset.profile_id ? `${dataset.profile_id}@${dataset.profile_version ?? 1}` : "");
  const [connection, setConnection] = useState(dataset.storage_connection_id ?? "");
  const tags = parseTags(tagText);
  const meta = parseKeyValues(metaText);
  const problems = {
    tags: tags.error ?? undefined,
    metadata: meta.error ?? undefined,
    description: description.length > 2000 ? "Keep the description to 2000 characters." : undefined,
  };
  const valid = !problems.tags && !problems.metadata && !problems.description;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;

    // Metadata is a merge patch: keys left out of the form are removed with null.
    const patch: Record<string, string | null> = { ...meta.values };
    for (const key of Object.keys(dataset.metadata)) {
      if (!(key in meta.values)) patch[key] = null;
    }

    const body: UpdateDatasetRequest = { revision: dataset.revision, description: description.trim(), tags: tags.tags, metadata: patch };
    const currentProfile = dataset.profile_id ? `${dataset.profile_id}@${dataset.profile_version ?? 1}` : "";
    if (profile && profile !== currentProfile) {
      const [id, version] = profile.split("@");
      body.profile_id = id;
      body.profile_version = Number(version);
    }
    if (connection.trim() !== (dataset.storage_connection_id ?? "")) body.storage_connection_id = connection.trim();
    onSave(body);
  }

  const canUseStorage = can("storage.use");

  return (
    <form onSubmit={submit} noValidate className="grid gap-4" aria-label="Edit dataset">
      <Field label="Description" error={problems.description}>
        {(props) => (
          <Textarea {...props} value={description} onChange={(e) => setDescription(e.target.value)} maxLength={2000} />
        )}
      </Field>
      <Field label="Tags" error={problems.tags} hint="Separate with commas or new lines.">
        {(props) => <Input {...props} value={tagText} onChange={(e) => setTagText(e.target.value)} />}
      </Field>
      <Field label="Metadata" error={problems.metadata} hint="One key=value per line. Remove a line to remove the key.">
        {(props) => (
          <Textarea {...props} value={metaText} onChange={(e) => setMetaText(e.target.value)} className="mono-data" />
        )}
      </Field>
      <Field label="Default validation profile">
        {(props) => (
          <Select {...props} value={profile} onChange={(e) => setProfile(e.target.value)}>
            <option value="">{dataset.profile_id ? "Keep the current profile" : "No default profile"}</option>
            {profiles.data?.profiles.map((p) => (
              <option key={`${p.id}@${p.version}`} value={`${p.id}@${p.version}`}>
                {p.name} (version {p.version})
              </option>
            ))}
          </Select>
        )}
      </Field>
      <Field
        label="Storage connection id"
        hint={
          canUseStorage
            ? "Leave empty to use the organization default."
            : "Changing the storage connection needs the storage.use permission."
        }
      >
        {(props) => (
          <Input
            {...props}
            value={connection}
            onChange={(e) => setConnection(e.target.value)}
            disabled={!canUseStorage}
            spellCheck={false}
            className="mono-data"
          />
        )}
      </Field>
      {Boolean(error) && (
        <p role="alert" className="text-base font-medium">
          {editMessage(error)}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <Button onClick={onCancel} disabled={pending}>
          Cancel
        </Button>
        <SubmitButton valid={valid} loading={pending} hint="Fix the highlighted fields to save.">
          Save changes
        </SubmitButton>
      </div>
    </form>
  );
}
