"use client";

import { CaretDown, CaretUp } from "@phosphor-icons/react";
import Link from "next/link";
import { memo, useCallback, useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Textarea } from "@/components/ui/field";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import type { StorageConnection } from "@/features/storage-connections/types";
import { cn } from "@/lib/utils/cn";
import { formatBytes } from "@/lib/utils/format";
import type { DraftErrors, FileDraft } from "../draft";
import type { UploadSnapshot, UploadState } from "../types";
import { ConnectionSelect, describeConnection } from "./connection-select";
import type { QueueEntry } from "./use-upload-queue";

const STATE_LABEL: Record<UploadState, string> = {
  queued: "Queued",
  hashing: "Reading file",
  uploading: "Uploading",
  finalizing: "Finishing upload",
  verifying: "Verifying",
  processing: "Processing",
  available: "Available",
  rejected: "Rejected",
  failed: "Failed",
  cancelled: "Cancelled",
};

// A queued file is one of three things, and each needs saying: it waits for
// Start, for a free slot, or for a fix to its details.
type QueuedAs = { label: string; text: string };

function queuedAs(hasProblem: boolean, ahead: number): QueuedAs {
  if (hasProblem) return { label: "Needs a fix", text: "Fix the problem shown under the file name, then start it." };
  if (ahead === 0) return { label: "Waiting", text: "Next in line for a free upload slot" };
  if (ahead > 0) return { label: "Waiting", text: `Waiting for a free upload slot, ${ahead} ${ahead === 1 ? "file" : "files"} ahead` };
  return { label: "Ready", text: "Ready. Start upload begins it." };
}

function badgeTone(state: UploadState) {
  if (state === "available") return "done" as const;
  if (state === "rejected" || state === "failed") return "attention" as const;
  if (state === "queued" || state === "cancelled") return "neutral" as const;
  return "active" as const;
}

export function ProgressBar({ label, value, max, text }: { label: string; value: number; max: number; text: string }) {
  const ratio = max > 0 ? Math.min(value / max, 1) : 0;
  return (
    <div className="min-w-48">
      <div
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={Math.min(value, max)}
        aria-valuetext={text}
        className="neo-in-sm h-3 p-0.5"
      >
        <div className="h-full origin-left bg-ink transition-transform duration-150 ease-out" style={{ transform: `scaleX(${ratio})` }} />
      </div>
      <p className="mt-1 text-sm text-muted tabular-nums">{text}</p>
    </div>
  );
}

function UploadProgress({ snap }: { snap: UploadSnapshot }) {
  if (snap.state === "hashing") {
    const percent = Math.round((snap.hashedBytes / Math.max(snap.fileSize, 1)) * 100);
    return <ProgressBar label={`Checksum of ${snap.fileName}`} value={snap.hashedBytes} max={snap.fileSize} text={`Reading file for its checksum, ${percent}%`} />;
  }
  // A large file's checksum is read while it uploads; when the parts finish
  // first, completion waits for it.
  if (snap.state === "finalizing" && snap.mode === "multipart" && snap.hashedBytes < snap.fileSize) {
    const percent = Math.round((snap.hashedBytes / Math.max(snap.fileSize, 1)) * 100);
    return <ProgressBar label={`Checksum of ${snap.fileName}`} value={snap.hashedBytes} max={snap.fileSize} text={`Finishing the checksum, ${percent}%`} />;
  }

  const done = ["finalizing", "verifying", "processing", "available", "rejected"].includes(snap.state);
  const sent = done ? snap.fileSize : snap.sentBytes;
  const percent = snap.fileSize > 0 ? Math.floor((sent / snap.fileSize) * 100) : 0;
  const parts = snap.mode === "multipart" && snap.partsTotal > 0 ? `, ${snap.partsDone} of ${snap.partsTotal} parts confirmed` : "";
  const speed = snap.state === "uploading" && snap.speedBps > 0 ? `, ${formatBytes(snap.speedBps)}/s` : "";
  const text = snap.state === "queued" ? "Not started" : `${percent}%, ${formatBytes(sent)} of ${formatBytes(snap.fileSize)}${speed}${parts}`;

  return <ProgressBar label={`Upload of ${snap.fileName}`} value={sent} max={snap.fileSize} text={text} />;
}

function Processing({ snap, queued }: { snap: UploadSnapshot; queued: QueuedAs }) {
  const { state, asset } = snap;
  let text = queued.text;
  if (state === "finalizing") text = "Waiting for the upload to finish";
  if (state === "verifying") text = "The server is verifying the checksum";
  if (state === "processing") text = asset?.status === "QUARANTINED" ? "Held in quarantine until the checks finish" : "Being checked and scanned";
  if (state === "available") text = "Available in the catalog";
  if (state === "rejected") text = `Rejected and kept in quarantine${asset?.rejectionReason ? `: ${asset.rejectionReason.replaceAll("_", " ")}` : ""}`;
  if (state === "failed" || state === "cancelled") text = "Not processed";

  return (
    <div className="max-w-64 text-sm">
      <p>{text}</p>
      {snap.pollingStopped && ["finalizing", "verifying", "processing"].includes(state) && (
        <p className="mt-1 text-muted">Checking paused. It may still be working; check again.</p>
      )}
      {snap.note && <p className="mt-1 text-muted">{snap.note}</p>}
      {asset?.objectId && (
        <Link href={`/app/catalog/${asset.objectId}`} className="mt-1 inline-block font-medium underline underline-offset-2">
          Open in catalog
        </Link>
      )}
    </div>
  );
}

function RowActions({ entry, snap, onRemove, onAskCancel }: { entry: QueueEntry; snap: UploadSnapshot; onRemove: () => void; onAskCancel: () => void }) {
  const { task } = entry;
  const kind = snap.failure?.kind;

  return (
    <div className="flex flex-wrap gap-2">
      {snap.state === "queued" && <Button onClick={onRemove}>Remove</Button>}
      {["hashing", "uploading", "finalizing"].includes(snap.state) && <Button onClick={onAskCancel}>Cancel</Button>}
      {["verifying", "processing"].includes(snap.state) && snap.pollingStopped && (
        <Button onClick={() => task.checkNow()}>Check again</Button>
      )}
      {["verifying", "processing"].includes(snap.state) && !snap.pollingStopped && (
        <Button onClick={() => task.stopWatching()}>Stop checking</Button>
      )}
      {snap.state === "failed" && (kind === "session_expired" || kind === "size_mismatch") && (
        <Button variant="primary" onClick={() => task.restart()}>
          Start again
        </Button>
      )}
      {snap.state === "failed" && (kind === "other" || kind === "cors" || kind === "quota") && (
        <Button variant="primary" onClick={() => task.retry()}>
          Retry
        </Button>
      )}
      {["failed", "cancelled", "available", "rejected"].includes(snap.state) && <Button onClick={onRemove}>Remove</Button>}
    </div>
  );
}

function Editor({
  draft,
  errors,
  connections,
  defaultConnectionId,
  onChange,
}: {
  draft: FileDraft;
  errors: DraftErrors;
  connections: StorageConnection[];
  defaultConnectionId: string;
  onChange: (change: Partial<FileDraft>) => void;
}) {
  return (
    <div className="grid gap-4 md:grid-cols-2">
      <Field label="Dataset name" hint="Optional. Groups the object in a dataset namespace." error={errors.dataset}>
        {(props) => <Input {...props} value={draft.dataset} spellCheck={false} autoComplete="off" onChange={(e) => onChange({ dataset: e.target.value })} />}
      </Field>
      <Field label="Catalog path" hint="Defaults to the file name. Use / for folders." error={errors.path}>
        {(props) => <Input {...props} value={draft.path} spellCheck={false} autoComplete="off" onChange={(e) => onChange({ path: e.target.value })} />}
      </Field>
      <Field label="Metadata" hint="Optional. One key=value per line." error={errors.metadata}>
        {(props) => <Textarea {...props} value={draft.metadataText} spellCheck={false} onChange={(e) => onChange({ metadataText: e.target.value })} />}
      </Field>
      <div className="flex flex-col gap-4">
        <ConnectionSelect
          label="Destination storage"
          connections={connections}
          defaultConnectionId={defaultConnectionId}
          value={draft.connectionId}
          onChange={(connectionId) => onChange({ connectionId })}
        />
        <Checkbox
          label="Only create: fail if an object already exists at this path"
          checked={draft.createOnly}
          onChange={(e) => onChange({ createOnly: e.target.checked })}
        />
      </div>
    </div>
  );
}

type Props = {
  entries: QueueEntry[];
  snapshots: UploadSnapshot[];
  errors: Map<string, DraftErrors>;
  connections: StorageConnection[];
  defaultConnectionId: string;
  onDraft: (id: string, change: Partial<FileDraft>) => void;
  onRemove: (id: string) => void;
  waitingIds: string[];
};

type RowProps = {
  entry: QueueEntry;
  snap: UploadSnapshot;
  rowErrors: DraftErrors;
  connection: StorageConnection | undefined;
  connections: StorageConnection[];
  defaultConnectionId: string;
  expanded: boolean;
  ahead: number;
  onToggle: (id: string) => void;
  onDraft: (id: string, change: Partial<FileDraft>) => void;
  onRemove: (id: string) => void;
  onAskCancel: (entry: QueueEntry) => void;
};

// One file's row. Memoized: every update makes a new snapshot for that file
// only, so a busy batch re-renders the rows that changed, not all of them.
const QueueRow = memo(function QueueRow({
  entry,
  snap,
  rowErrors,
  connection,
  connections,
  defaultConnectionId,
  expanded,
  ahead,
  onToggle,
  onDraft,
  onRemove,
  onAskCancel,
}: RowProps) {
  const editable = snap.state === "queued";
  // The open editor already shows its own field errors.
  const problems = Object.values(expanded ? { size: rowErrors.size } : rowErrors).filter(Boolean);
  const queued = queuedAs(Object.values(rowErrors).some(Boolean), ahead);

  return (
    <>
      <tr>
        <Td className="min-w-48 max-w-72">
          <p className="font-medium [overflow-wrap:anywhere]">{snap.fileName}</p>
          <p className="text-sm text-muted">
            {formatBytes(snap.fileSize)}
            {snap.request.path ? `, ${snap.request.path}` : ""}
            {snap.request.dataset ? `, dataset ${snap.request.dataset}` : ""}
          </p>
          <p className="text-sm text-muted">To {describeConnection(connection, snap.storageConnectionId)}</p>
          {editable && problems.length > 0 && (
            <p role="alert" className="mt-1 text-sm font-medium">
              {problems[0]}
            </p>
          )}
          {editable && (
            <button
              type="button"
              aria-expanded={expanded}
              aria-controls={`edit-${entry.id}`}
              onClick={() => onToggle(entry.id)}
              className="mt-1 inline-flex items-center gap-1 text-sm font-medium underline underline-offset-2"
            >
              {expanded ? <CaretUp aria-hidden /> : <CaretDown aria-hidden />}
              {expanded ? "Hide details" : "Edit details"}
            </button>
          )}
        </Td>
        <Td>
          <UploadProgress snap={snap} />
        </Td>
        <Td>
          <Processing snap={snap} queued={queued} />
        </Td>
        <Td className="max-w-72">
          <StateBadge tone={badgeTone(snap.state)}>{snap.state === "queued" ? queued.label : STATE_LABEL[snap.state]}</StateBadge>
          {snap.failure && (
            <p role="alert" className={cn("mt-2 text-sm")}>
              {snap.failure.message}
            </p>
          )}
        </Td>
        <Td>
          <RowActions entry={entry} snap={snap} onRemove={() => onRemove(entry.id)} onAskCancel={() => onAskCancel(entry)} />
        </Td>
      </tr>
      {expanded && (
        <tr>
          <Td colSpan={5} className="bg-faint/40">
            <div id={`edit-${entry.id}`}>
              <Editor
                draft={entry.draft}
                errors={rowErrors}
                connections={connections}
                defaultConnectionId={defaultConnectionId}
                onChange={(change) => onDraft(entry.id, change)}
              />
            </div>
          </Td>
        </tr>
      )}
    </>
  );
});

const NO_ERRORS: DraftErrors = {};

export function QueueTable({ entries, snapshots, errors, connections, defaultConnectionId, onDraft, onRemove, waitingIds }: Props) {
  const [open, setOpen] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState<QueueEntry | null>(null);
  const toggle = useCallback((id: string) => setOpen((current) => (current === id ? null : id)), []);

  return (
    <>
      <Table aria-label="Upload queue">
        <thead>
          <tr>
            <Th>File</Th>
            <Th>Upload</Th>
            <Th>Processing</Th>
            <Th>State</Th>
            <Th>Actions</Th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry, index) => {
            const snap = snapshots[index];
            if (!snap) return null;
            return (
              <QueueRow
                key={entry.id}
                entry={entry}
                snap={snap}
                rowErrors={errors.get(entry.id) ?? NO_ERRORS}
                connection={connections.find((c) => c.id === (snap.storageConnectionId ?? entry.draft.connectionId))}
                connections={connections}
                defaultConnectionId={defaultConnectionId}
                expanded={snap.state === "queued" && open === entry.id}
                ahead={waitingIds.indexOf(entry.id)}
                onToggle={toggle}
                onDraft={onDraft}
                onRemove={onRemove}
                onAskCancel={setCancelling}
              />
            );
          })}
        </tbody>
      </Table>

      <ConfirmDialog
        open={cancelling !== null}
        onOpenChange={(next) => !next && setCancelling(null)}
        title="Cancel this upload?"
        description={`The upload of ${cancelling?.task.file.name ?? "this file"} stops and the parts already sent are discarded. You can add the file again later.`}
        confirmLabel="Cancel upload"
        onConfirm={() => cancelling?.task.cancel()}
      />
    </>
  );
}
