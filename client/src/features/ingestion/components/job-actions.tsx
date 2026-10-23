"use client";

import { ArrowClockwise, Prohibit } from "@phosphor-icons/react";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { describeError, isApiError } from "@/lib/api/errors";
import { formatNumber } from "@/lib/utils/format";
import { cancelJob, finalizeJob, retryEntries } from "../api";
import { useRefreshJob } from "../hooks";
import { buildRetryBody } from "../lib/retry-body";
import type { IngestionJob } from "../types";

const CANCELLABLE = ["PENDING_VALIDATION", "VALIDATING", "READY", "RUNNING"];

export function FinalizePanel({ job, canWrite }: { job: IngestionJob; canWrite: boolean }) {
  const refresh = useRefreshJob();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const waiting = !job.manifest_complete;

  async function finalize() {
    setBusy(true);
    setError(null);
    try {
      await finalizeJob(job.id);
      await refresh();
    } catch (e) {
      setError(
        isApiError(e) && e.status === 409
          ? "The server has not finished reading the manifest. Wait a moment and try again."
          : isApiError(e) && e.status === 403
            ? `${e.message} The files may not fit the storage or dataset quota.`
            : describeError(e),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel title="Finalize" description="Finalizing opens the job for uploads. Entries cannot be uploaded before this.">
      <div className="flex flex-col items-start gap-3">
        <p className="text-sm">
          {waiting
            ? `The server is still reading the manifest: ${formatNumber(job.parsed_lines)} lines so far.`
            : `The manifest is fully read: ${formatNumber(job.progress.total)} lines, ${formatNumber(job.progress.valid)} valid, ${formatNumber(job.progress.invalid)} invalid.`}
        </p>
        {!waiting && job.progress.invalid > 0 && (
          <p className="text-sm text-muted">Invalid lines do not block finalizing. They stay as INVALID entries with a reason.</p>
        )}
        {error && (
          <p role="alert" className="font-medium">
            {error}
          </p>
        )}
        <Button variant="primary" onClick={finalize} loading={busy} disabled={waiting || !canWrite}>
          Finalize job
        </Button>
      </div>
    </Panel>
  );
}

export function JobActions({ job, canManage }: { job: IngestionJob; canManage: boolean }) {
  const refresh = useRefreshJob();
  const [cancelOpen, setCancelOpen] = useState(false);
  const [retryOpen, setRetryOpen] = useState(false);
  const [text, setText] = useState("");
  const [retryError, setRetryError] = useState<string | null>(null);
  const [retryResult, setRetryResult] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const failedCount = job.progress.failed + job.progress.failed_retryable;
  const canRetry = canManage && failedCount > 0 && job.state !== "CANCELLING" && job.state !== "CANCELLED";
  const canCancel = canManage && CANCELLABLE.includes(job.state);

  async function retry() {
    const body = buildRetryBody(text);
    if ("error" in body) {
      setRetryError(body.error);
      return;
    }
    setBusy(true);
    setRetryError(null);
    try {
      const result = await retryEntries(job.id, body);
      setRetryResult(`${formatNumber(result.retried)} entries go back to the queue for a new upload, and ${formatNumber(result.requeued)} are requeued for processing without uploading again.`);
      setRetryOpen(false);
      setText("");
      await refresh();
    } catch (e) {
      setRetryError(
        isApiError(e) && e.status === 403
          ? `${e.message} Retrying makes a finished job active again, and your organization may be at its limit of active jobs.`
          : describeError(e),
      );
    } finally {
      setBusy(false);
    }
  }

  if (!canRetry && !canCancel && !retryResult) return null;

  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex flex-wrap gap-2">
        {canRetry && (
          <Button onClick={() => setRetryOpen(true)}>
            <ArrowClockwise aria-hidden />
            Retry failed
          </Button>
        )}
        {canCancel && (
          <Button onClick={() => setCancelOpen(true)}>
            <Prohibit aria-hidden />
            Cancel job
          </Button>
        )}
      </div>
      {retryResult && (
        <p role="status" className="max-w-md text-right text-sm">
          {retryResult}
        </p>
      )}

      <Modal
        open={retryOpen}
        onOpenChange={setRetryOpen}
        title="Retry failed entries"
        description="Only entries with a retryable reason go back to the queue. Each entry can be retried by hand up to 3 times. Published entries are never touched."
        footer={
          <>
            <Button onClick={() => setRetryOpen(false)}>Close</Button>
            <Button variant="primary" onClick={retry} loading={busy}>
              {text.trim() ? "Retry these entries" : "Retry all failed"}
            </Button>
          </>
        }
      >
        <Field label="Entry numbers" hint="Leave empty to retry every failed entry. Or list up to 1000 numbers separated by commas or spaces.">
          {(props) => <Input {...props} value={text} onChange={(e) => setText(e.target.value)} inputMode="numeric" placeholder="3, 17, 204" />}
        </Field>
        {retryError && (
          <p role="alert" className="mt-3 font-medium">
            {retryError}
          </p>
        )}
      </Modal>

      <ConfirmDialog
        open={cancelOpen}
        onOpenChange={setCancelOpen}
        title="Cancel this job?"
        confirmLabel="Cancel job"
        description={
          <>
            <p>Waiting entries are cancelled and the job stops handing out upload slots. Cancelling a large job runs in the background and can take a while.</p>
            <p className="mt-2">
              Files that are already stored still finish processing and are published. Deleting assets that were already published is a separate action and is not done here.
            </p>
          </>
        }
        onConfirm={async () => {
          await cancelJob(job.id);
          await refresh();
        }}
      />
    </div>
  );
}
