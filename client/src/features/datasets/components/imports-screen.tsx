"use client";

import { useRouter } from "next/navigation";
import { useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { PermissionDenied } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { newIdempotencyKey } from "@/lib/api/client";
import { useSession } from "@/lib/auth/session";
import { putBlob } from "@/lib/upload/put";
import { sha256Blob } from "@/lib/upload/sha256";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import * as api from "../api";
import {
  importMessage,
  INLINE_LIMIT_BYTES,
  preflightManifest,
  type PreflightResult,
} from "../helpers";
import { useDataset, useRefreshDatasets } from "../hooks";
import { ImportHistory } from "./import-history";
import type { DuplicatePolicy } from "../types";

type Mode = "inline" | "staged";
type Step = "idle" | "hashing" | "sending" | "uploading" | "submitting";

const stepLabels: Record<Step, string> = {
  idle: "",
  hashing: "Computing the file checksum",
  sending: "Starting the import",
  uploading: "Uploading the manifest to storage",
  submitting: "Submitting the import",
};

const example = `{"object_id":"<object id>","version_id":"<asset id>","label":"cat","split":"train","metadata":{"annotator":"a1"}}
{"namespace":"photos","path":"2024/cat-17.jpg","version":3,"label":"cat","schema_version":1}`;

export function ImportsScreen({ id }: { id: string }) {
  const { can } = useSession();
  const dataset = useDataset(id);
  const archived = dataset.data?.lifecycle_status === "ARCHIVED";

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="grid content-start gap-6 lg:col-span-2">
        {!can("datasets.write") ? (
          <PermissionDenied detail="Importing a manifest needs the datasets.write permission." />
        ) : archived ? (
          <Panel title="Imports are closed">This dataset is archived and read-only, so it cannot take imports.</Panel>
        ) : (
          <StartImport datasetId={id} />
        )}

        <ImportHistory id={id} />
      </div>

      <Panel title="Manifest format" description="JSON Lines, schema version 1. One member per line.">
        <pre className="mono-data neo-in-sm overflow-x-auto p-3 whitespace-pre" data-lenis-prevent-horizontal>
          {example}
        </pre>
        <ul className="mt-4 grid gap-2 text-base">
          <li>Give object_id, or path with an optional namespace. Not both.</li>
          <li>Give version_id (the asset id) or version (the number) to pin. Leave both out to follow the current version.</li>
          <li>Optional: label (up to 256 characters), split (train, validation or test), metadata (flat strings).</li>
          <li>schema_version, when present, must be 1.</li>
          <li>Blank lines are skipped. Unknown fields and tenant_id are refused.</li>
          <li>Each line can be at most 64 KiB.</li>
        </ul>
        <p className="mt-4 text-sm text-muted">
          Importing only reads references and metadata. A bad line is rejected and reported; the good lines still
          import. Members already added stay if you cancel.
        </p>
      </Panel>
    </div>
  );
}

function StartImport({ datasetId }: { datasetId: string }) {
  const router = useRouter();
  const refresh = useRefreshDatasets();
  const [file, setFile] = useState<File | null>(null);
  const [check, setCheck] = useState<PreflightResult | null>(null);
  const [checking, setChecking] = useState(false);
  const [policy, setPolicy] = useState<DuplicatePolicy>("REJECT");
  const [mode, setMode] = useState<Mode>("inline");
  const [step, setStep] = useState<Step>("idle");
  const [progress, setProgress] = useState(0);
  const [error, setError] = useState<string | null>(null);
  // One key per file choice; retrying the same attempt reuses it.
  const key = useRef(newIdempotencyKey());
  const abort = useRef<AbortController | null>(null);

  async function choose(event: ChangeEvent<HTMLInputElement>) {
    const picked = event.target.files?.[0] ?? null;
    setFile(picked);
    setCheck(null);
    setError(null);
    key.current = newIdempotencyKey();
    if (!picked) return;
    setMode(picked.size > INLINE_LIMIT_BYTES ? "staged" : "inline");
    setChecking(true);
    try {
      setCheck(await preflightManifest(picked));
    } catch {
      setError("The file could not be read. Choose it again.");
    } finally {
      setChecking(false);
    }
  }

  async function start(event: FormEvent) {
    event.preventDefault();
    if (!file || !valid) return;
    setError(null);
    abort.current = new AbortController();
    try {
      setStep("hashing");
      const sha256 = await sha256Blob(file, { signal: abort.current.signal });
      setStep("sending");
      let job;
      if (mode === "inline") {
        job = await api.startInlineImport(datasetId, file, key.current, policy, sha256);
      } else {
        job = await api.startStagedImport(datasetId, key.current, file.size, policy, sha256);
        if (job.state === "AWAITING_UPLOAD" && job.upload_url) {
          setStep("uploading");
          setProgress(0);
          await putBlob(job.upload_url, file, {
            headers: { "Content-Type": "application/x-ndjson" },
            signal: abort.current.signal,
            onProgress: setProgress,
          });
          setStep("submitting");
          job = await api.submitImport(datasetId, job.id);
        }
      }
      refresh();
      router.push(`/app/datasets/${datasetId}/imports/${job.id}`);
    } catch (e) {
      if (e instanceof DOMException && e.name === "AbortError") setError("Stopped. Nothing was imported.");
      else setError(importMessage(e));
      setStep("idle");
    }
  }

  const busy = step !== "idle";
  const tooBigForInline = Boolean(file) && file!.size > INLINE_LIMIT_BYTES;
  let fileProblem: string | undefined;
  if (!file) fileProblem = "Choose a manifest file.";
  else if (check && check.lines === 0) fileProblem = "This file has no lines. Choose a manifest with at least one member.";
  else if (mode === "inline" && tooBigForInline) fileProblem = "This file is over 8 MiB. Use the staged upload.";
  const valid = !fileProblem && !checking;
  const hint = checking ? "Wait for the file check to finish." : (fileProblem ?? "");

  return (
    <Panel
      title="Import a manifest"
      description="Add many members at once from a JSON Lines file. The import runs in the background and you can follow it."
    >
      <form onSubmit={start} noValidate className="grid gap-4" aria-label="Import a manifest">
        <Field label="Manifest file" required error={fileProblem} hint="A .jsonl file, one JSON object per line.">
          {(props) => (
            <input
              {...props}
              type="file"
              accept=".jsonl,.ndjson,application/x-ndjson,application/jsonl,text/plain"
              onChange={choose}
              disabled={busy}
              className="neo-in-sm w-full p-2 text-base"
            />
          )}
        </Field>

        {checking && <p role="status" className="text-base text-muted">Checking the file, one line at a time.</p>}

        {check && file && (
          <div className="neo-in-sm p-4 text-base" aria-label="File check">
            <p className="font-semibold">
              {formatNumber(check.lines)} {check.lines === 1 ? "line" : "lines"}, {formatBytes(file.size)}.{" "}
              {check.invalid === 0
                ? "No problems found in this check."
                : `${formatNumber(check.invalid)} ${check.invalid === 1 ? "line has" : "lines have"} a problem.`}
            </p>
            <p className="mt-1 text-sm text-muted">
              This check looks at the shape of each line only. The server also checks that objects and versions exist.
            </p>
            {check.samples.length > 0 && (
              <>
                <ul className="mt-3 grid gap-1">
                  {check.samples.map((sample) => (
                    <li key={sample.line}>
                      <span className="mono-data">Line {sample.line}</span>: {sample.problem}
                    </li>
                  ))}
                </ul>
                {check.invalid > check.samples.length && (
                  <p className="mt-2 text-sm text-muted">Showing the first {check.samples.length}.</p>
                )}
                <p className="mt-2 text-sm text-muted">
                  You can still import. The server rejects these lines and imports the rest.
                </p>
              </>
            )}
          </div>
        )}

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Upload method" hint="Inline sends the file in the request, up to 8 MiB. Staged uploads it to storage first, for larger files.">
            {(props) => (
              <Select {...props} value={mode} onChange={(e) => setMode(e.target.value as Mode)} disabled={busy}>
                <option value="inline" disabled={tooBigForInline}>
                  Inline (up to 8 MiB)
                </option>
                <option value="staged">Staged upload</option>
              </Select>
            )}
          </Field>
          <Field label="If a member already exists">
            {(props) => (
              <Select {...props} value={policy} onChange={(e) => setPolicy(e.target.value as DuplicatePolicy)} disabled={busy}>
                <option value="REJECT">Reject the line</option>
                <option value="SKIP">Skip the line</option>
                <option value="REPLACE">Replace the member</option>
              </Select>
            )}
          </Field>
        </div>

        {busy && (
          <div role="status" className="neo-in-sm p-4 text-base">
            {stepLabels[step]}
            {step === "uploading" && file && (
              <span className="ml-2 tabular-nums">
                {formatBytes(progress)} of {formatBytes(file.size)}
              </span>
            )}
          </div>
        )}
        {error && (
          <p role="alert" className="text-base font-medium">
            {error}
          </p>
        )}

        <div className="flex flex-wrap gap-2">
          <SubmitButton valid={valid} loading={busy} hint={hint}>
            {error ? "Try again" : "Start import"}
          </SubmitButton>
          {(step === "hashing" || step === "uploading") && <Button onClick={() => abort.current?.abort()}>Stop</Button>}
        </div>
      </form>
    </Panel>
  );
}
