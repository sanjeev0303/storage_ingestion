"use client";

import { useRef, useState, type FormEvent } from "react";
import { CopyButton } from "@/components/ui/copy-button";
import { Field } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { describeError, isApiError } from "@/lib/api/errors";
import { isAbort } from "@/lib/upload/sha256";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { useManifestLimits, useRefreshJob } from "../hooks";
import { explainError } from "../lib/explain";
import { manifestMode, submitManifest, type SubmitStep } from "../lib/manifest";
import { exceededLimits, preflightManifest, type PreflightReport } from "../lib/preflight";
import type { ManifestReport } from "../types";

export const EXAMPLE_LINE =
  '{"path":"images/cat.jpg","size":48213,"content_type":"image/jpeg","checksum":"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","required":true,"metadata":{"camera":"a7"}}';

const FIELDS = [
  ["path", "Required", "Relative path inside the dataset, such as images/cat.jpg. Unique without regard to case. No .., backslashes, leading slash or reserved characters."],
  ["size", "Required", 'Whole number of bytes, 1 or more. The key is "size", not "size_bytes".'],
  ["content_type", "Required", "A media type such as image/jpeg. Uploads are sent with exactly this Content-Type."],
  ["checksum", "Required", '"sha256:" followed by 64 hex characters. A line without one can never be published.'],
  ["required", "Optional", "true or false, default true. Used by datasets whose policy is REQUIRED."],
  ["metadata", "Optional", "A flat object of string values, up to 32 keys of letters, digits, dot, dash or underscore."],
] as const;

function describeSubmitError(error: unknown) {
  if (isApiError(error)) {
    if (error.status === 409) return "A different manifest was already submitted for this job. A job takes one manifest; create a new job to use another file.";
    if (error.status === 422) return "The manifest did not match its SHA-256 checksum, so it was damaged on the way. Try the upload again.";
    if (error.status === 413) return `The manifest is over a limit and the job was failed. ${error.message}`;
  }
  return describeError(error);
}

const STEP_TEXT: Record<SubmitStep, string> = {
  hashing: "Computing the manifest checksum",
  sending: "Sending the manifest",
  uploading: "Uploading the manifest to storage",
  "handing-over": "Telling the server the upload is done",
};

export function ManifestFormat() {
  return (
    <div className="flex flex-col gap-4">
      <p className="max-w-prose text-base">
        A manifest is a JSON Lines file: UTF-8, one JSON object per line, one line per file. Line 1 becomes entry 1. Blank lines are skipped but still count. CSV is not supported. Unknown keys are ignored.
      </p>
      <Table>
        <caption className="sr-only">Manifest line keys</caption>
        <thead>
          <tr>
            <Th>Key</Th>
            <Th>Needed</Th>
            <Th>Rule</Th>
          </tr>
        </thead>
        <tbody>
          {FIELDS.map(([key, needed, rule]) => (
            <tr key={key}>
              <Td className="mono-data">{key}</Td>
              <Td>{needed}</Td>
              <Td className="min-w-72 whitespace-normal">{rule}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
      <div className="neo-in-sm p-3">
        <p className="mono-data break-all text-sm" data-lenis-prevent>
          {EXAMPLE_LINE}
        </p>
      </div>
      <div>
        <CopyButton value={EXAMPLE_LINE} label="Copy example line" />
      </div>
      <p className="max-w-prose text-sm text-muted">
        A line with a problem does not stop the manifest. It becomes an INVALID entry with an error code, and the other lines carry on. A line may be at most 16,384 bytes.
      </p>
    </div>
  );
}

export function ReportView({ report }: { report: ManifestReport }) {
  return (
    <div className="flex flex-col gap-3" role="status">
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
        {(
          [
            ["Lines read", report.lines],
            ["Accepted", report.accepted],
            ["Invalid", report.invalid],
            ["Duplicates", report.duplicates],
            ["Replayed", report.replayed],
          ] as const
        ).map(([label, value]) => (
          <div key={label}>
            <dt className="text-sm text-muted">{label}</dt>
            <dd className="text-lg font-semibold tabular-nums">{formatNumber(value)}</dd>
          </div>
        ))}
      </dl>
      <p className="text-sm text-muted">{report.complete ? "The server has read the whole manifest." : "The server has not finished reading the manifest."}</p>
      {report.invalid_samples && report.invalid_samples.length > 0 && (
        <Table>
          <caption className="sr-only">First invalid lines</caption>
          <thead>
            <tr>
              <Th>Line</Th>
              <Th>Error</Th>
              <Th>Detail</Th>
            </tr>
          </thead>
          <tbody>
            {report.invalid_samples.map((entry) => (
              <tr key={entry.position}>
                <Td className="tabular-nums">{entry.position}</Td>
                <Td className="mono-data">{entry.error_code}</Td>
                <Td className="min-w-64 whitespace-normal">{entry.error_detail || explainError(entry.error_code)}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </div>
  );
}

function PreflightView({ report, limits }: { report: PreflightReport; limits: { entries?: number; bytes?: number } | null }) {
  const over = limits ? exceededLimits(report, { maxEntries: limits.entries, maxBytes: limits.bytes }) : null;
  return (
    <div className="flex flex-col gap-3">
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <div>
          <dt className="text-sm text-muted">Lines</dt>
          <dd className="text-lg font-semibold tabular-nums">{formatNumber(report.lines)}</dd>
        </div>
        <div>
          <dt className="text-sm text-muted">Lines with problems</dt>
          <dd className="text-lg font-semibold tabular-nums">{formatNumber(report.problemLines)}</dd>
        </div>
        <div>
          <dt className="text-sm text-muted">Bytes the files add up to</dt>
          <dd className="text-lg font-semibold tabular-nums">{formatBytes(report.totalEntryBytes)}</dd>
        </div>
        <div>
          <dt className="text-sm text-muted">Manifest file</dt>
          <dd className="text-lg font-semibold tabular-nums">{formatBytes(report.fileBytes)}</dd>
        </div>
      </dl>
      {!report.duplicatesFullyChecked && (
        <p className="text-sm text-muted">
          The browser checked the first 200,000 paths for duplicates. The server checks the rest.
        </p>
      )}
      {limits ? (
        <p className="text-sm" role="status">
          {over?.entries && `This manifest has ${formatNumber(report.lines)} lines; your organization allows ${formatNumber(limits.entries)}. `}
          {over?.bytes && `This manifest is ${formatBytes(report.fileBytes)}; your organization allows ${formatBytes(limits.bytes)}. `}
          {!over?.entries && !over?.bytes && "Within your organization's manifest limits."}
        </p>
      ) : (
        <p className="text-sm text-muted">Your organization&apos;s manifest limits could not be read, so they were not checked.</p>
      )}
      {report.samples.length > 0 && (
        <Table>
          <caption className="sr-only">First problems found</caption>
          <thead>
            <tr>
              <Th>Line</Th>
              <Th>Problem</Th>
            </tr>
          </thead>
          <tbody>
            {report.samples.map((sample, i) => (
              <tr key={`${sample.line}-${i}`}>
                <Td className="tabular-nums">{sample.line}</Td>
                <Td className="min-w-64 whitespace-normal">{sample.problem}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {report.problemLines > report.samples.length && (
        <p className="text-sm text-muted">Showing the first {report.samples.length} of {formatNumber(report.problemLines)} problems.</p>
      )}
    </div>
  );
}

export function ManifestPanel({ jobId, canWrite }: { jobId: string; canWrite: boolean }) {
  const refresh = useRefreshJob();
  const quota = useManifestLimits();
  const [file, setFile] = useState<File | null>(null);
  const [checking, setChecking] = useState(false);
  const [preflight, setPreflight] = useState<PreflightReport | null>(null);
  const [step, setStep] = useState<{ step: SubmitStep; fraction?: number } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [report, setReport] = useState<ManifestReport | null>(null);
  const [handedOver, setHandedOver] = useState(false);
  const abort = useRef<AbortController | null>(null);

  const limits = quota.data
    ? { entries: quota.data.limits.max_manifest_entries, bytes: quota.data.limits.max_manifest_bytes }
    : null;
  const over = preflight && limits ? exceededLimits(preflight, { maxEntries: limits.entries, maxBytes: limits.bytes }) : null;
  const overLimit = Boolean(over?.entries || over?.bytes);
  const fileError = overLimit ? "This manifest is over your organization's limit. Split it into smaller files." : undefined;
  const busy = step !== null;
  const valid = Boolean(file) && !checking && !overLimit && canWrite && !handedOver;
  const hint = !canWrite
    ? "Your role cannot submit manifests."
    : handedOver
      ? "The manifest is already submitted."
      : overLimit
        ? "Pick a manifest that is within the limits."
        : checking
          ? "Wait for the file check to finish."
          : "Choose a manifest file to submit.";

  async function pick(next: File | null) {
    abort.current?.abort();
    setFile(next);
    setPreflight(null);
    setError(null);
    setReport(null);
    if (!next) return;
    const controller = new AbortController();
    abort.current = controller;
    setChecking(true);
    try {
      setPreflight(await preflightManifest(next, { signal: controller.signal, onProgress: setPreflight }));
    } catch (e) {
      if (!isAbort(e)) setError("The browser could not read that file.");
    } finally {
      if (abort.current === controller) setChecking(false);
    }
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid || !file) return;
    const controller = new AbortController();
    abort.current = controller;
    setError(null);
    setStep({ step: "hashing", fraction: 0 });
    try {
      const result = await submitManifest(jobId, file, {
        signal: controller.signal,
        onStep: (s, fraction) => setStep({ step: s, fraction }),
      });
      if (result.mode === "inline") setReport(result.report);
      else setHandedOver(true);
      await refresh();
    } catch (e) {
      if (isApiError(e) && e.status === 413) {
        const body = e.body as { report?: ManifestReport } | undefined;
        if (body?.report) setReport(body.report);
      }
      if (!isAbort(e)) setError(describeSubmitError(e));
    } finally {
      setStep(null);
    }
  }

  const mode = file ? manifestMode(file.size) : null;

  return (
    <div className="flex flex-col gap-6">
      <Panel title="Manifest format" description="The manifest lists every file to load. Read this before you build one.">
        <ManifestFormat />
      </Panel>

      <Panel title="Submit the manifest" description="Check the file in your browser first. Nothing is sent until you press Submit.">
        <form onSubmit={submit} className="flex flex-col gap-5">
          <Field label="Manifest file" required error={fileError} hint="A .jsonl file. It is read as a stream, so very large files are fine.">
            {(props) => (
              <input
                {...props}
                type="file"
                accept=".jsonl,.ndjson,.json,.txt,application/x-ndjson"
                disabled={busy || !canWrite}
                onChange={(e) => pick(e.target.files?.[0] ?? null)}
                className="neo-in-sm w-full p-2 text-base"
              />
            )}
          </Field>

          {checking && (
            <p role="status" className="text-sm text-muted">
              Checking the file{preflight ? `: ${formatNumber(preflight.lines)} lines so far` : ""}
            </p>
          )}
          {preflight && <PreflightView report={preflight} limits={limits} />}

          {file && mode && (
            <p className="text-sm text-muted">
              {mode === "inline"
                ? "This file is 16 MiB or less, so it is sent to the API in one request."
                : "This file is over 16 MiB, so it is uploaded straight to storage and the server reads it from there. That can take a while for big files."}
            </p>
          )}
          {preflight && preflight.problemLines > 0 && !overLimit && (
            <p className="text-sm">
              Lines with problems do not stop the job. They become INVALID entries that you can see in the failures list.
            </p>
          )}

          {step && (
            <p role="status" className="font-medium">
              {STEP_TEXT[step.step]}
              {step.fraction !== undefined && step.fraction > 0 ? `: ${Math.round(step.fraction * 100)}%` : ""}
            </p>
          )}
          {error && (
            <p role="alert" className="font-medium">
              {error}
            </p>
          )}
          {handedOver && (
            <p role="status" className="font-medium">
              Uploaded. The server is now reading the manifest. This page updates when it is done.
            </p>
          )}

          <SubmitButton valid={valid} loading={busy} hint={hint}>
            Submit manifest
          </SubmitButton>
          {!canWrite && <p className="text-sm text-muted">Submitting needs the ingestion.write permission.</p>}
        </form>
        {report && (
          <div className="mt-6">
            <h3 className="mb-3 text-md font-semibold">Server report</h3>
            <ReportView report={report} />
          </div>
        )}
      </Panel>
    </div>
  );
}
