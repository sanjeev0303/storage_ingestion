import { MIB } from "@/lib/upload/parts";
import { isTransientPutError, putBlob } from "@/lib/upload/put";
import { withRetry } from "@/lib/upload/retry";
import { sha256Blob } from "@/lib/upload/sha256";
import { putManifestInline, requestStagedUrl, submitStaged } from "../api";
import type { IngestionJob, ManifestReport } from "../types";

export const INLINE_LIMIT_BYTES = 16 * MIB;

export function manifestMode(sizeBytes: number) {
  return sizeBytes <= INLINE_LIMIT_BYTES ? "inline" : "staged";
}

export type SubmitResult = { mode: "inline"; report: ManifestReport } | { mode: "staged"; job: IngestionJob };

export type SubmitStep = "hashing" | "sending" | "uploading" | "handing-over";

type SubmitOptions = {
  signal?: AbortSignal;
  onStep?: (step: SubmitStep, fraction?: number) => void;
};

export async function submitManifest(jobId: string, file: File, options: SubmitOptions = {}): Promise<SubmitResult> {
  const { signal, onStep } = options;

  // The checksum lets the server refuse a manifest that was damaged on the way.
  onStep?.("hashing", 0);
  const sha256 = await sha256Blob(file, { signal, onProgress: (n) => onStep?.("hashing", n / file.size) });

  if (manifestMode(file.size) === "inline") {
    onStep?.("sending");
    const report = await putManifestInline(jobId, file, sha256, signal);
    return { mode: "inline", report };
  }

  const { upload_url } = await requestStagedUrl(jobId, file.size);
  onStep?.("uploading", 0);
  await withRetry(
    () =>
      putBlob(upload_url, file, {
        headers: { "Content-Type": "application/x-ndjson" },
        signal,
        onProgress: (n) => onStep?.("uploading", n / file.size),
      }),
    { shouldRetry: isTransientPutError, signal },
  );
  onStep?.("handing-over");
  const job = await submitStaged(jobId, sha256);
  return { mode: "staged", job };
}
