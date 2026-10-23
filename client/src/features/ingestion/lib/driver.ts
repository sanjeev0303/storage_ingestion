import { describeError, isApiError } from "@/lib/api/errors";
import { reportProblem } from "@/lib/errors/report";
import { choosePartSize, planParts, runPool } from "@/lib/upload/parts";
import { isTransientPutError, MissingEtagError, UploadHttpError } from "@/lib/upload/put";
import { sleep as defaultSleep, withRetry } from "@/lib/upload/retry";
import type { ClaimResponse, Grant, IngestionEntry, LimitedBy, PartRecord, PartUrl } from "../types";

export const MAX_CLAIM = 100;
export const MAX_PART_BATCH = 100;
export const PART_LANES = 3;
export const BROWSER_CONCURRENCY_CAP = 6;
export const HEARTBEAT_MS = 5 * 60_000;
const DEFAULT_WAIT_SECONDS = 5;

export type DriverApi = {
  claim: (max: number, signal: AbortSignal) => Promise<ClaimResponse>;
  uploadUrl: (position: number) => Promise<{ upload_url?: string }>;
  partUrls: (position: number, numbers: number[]) => Promise<{ parts: PartUrl[] }>;
  confirmParts: (position: number, parts: PartRecord[]) => Promise<unknown>;
  heartbeat: (position: number) => Promise<unknown>;
  complete: (position: number, parts?: { part_number: number; etag: string }[]) => Promise<{ status: number; entry: IngestionEntry }>;
  getEntry: (position: number) => Promise<IngestionEntry>;
};

export type Putter = (
  url: string,
  body: Blob,
  options: { headers?: Record<string, string>; signal?: AbortSignal; onProgress?: (sent: number) => void },
) => Promise<{ status: number; etag: string | null }>;

export type DriverEvent =
  | { type: "claimed"; count: number; limitedBy?: LimitedBy }
  | { type: "waiting"; seconds: number; reason: string }
  | { type: "started"; position: number; path: string; size: number; strategy: string }
  | { type: "progress"; position: number; sent: number }
  | { type: "done"; position: number; state: string }
  | { type: "failed"; position: number; message: string }
  | { type: "skipped"; position: number; message: string };

export type DriverOptions = {
  api: DriverApi;
  put: Putter;
  getFile: (entry: IngestionEntry) => Blob | undefined;
  concurrency: number;
  // True when the connection is Azure Blob, which needs x-ms-blob-type on single PUTs.
  azure?: boolean;
  signal: AbortSignal;
  // Checked before every claim. When it turns true the driver lets running uploads finish and stops.
  shouldPause: () => boolean;
  onEvent: (event: DriverEvent) => void;
  heartbeatMs?: number;
  pollMs?: number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
};

// "unmatched": ten claimed entries in a row had no usable local file.
export type DriverEnd = "finished" | "paused" | "job_not_running" | "unmatched";
const MAX_SKIPS_IN_A_ROW = 10;

function messageOf(error: unknown) {
  if (error instanceof UploadHttpError) {
    const reference = reportProblem("upload", `provider answered ${error.status}: ${error.body}`);
    return `The storage provider refused the upload. The connection may not allow writing here. Check the connection, then try again. Reference ${reference}.`;
  }
  if (isApiError(error)) return describeError(error);
  const reference = reportProblem("upload", error instanceof Error ? `${error.name}: ${error.message}` : String(error));
  return `The upload stopped unexpectedly. Nothing you did caused it. Try again in a moment. Reference ${reference}.`;
}

export async function runDriver(options: DriverOptions): Promise<DriverEnd> {
  const { api, put, signal, onEvent } = options;
  const wait = options.sleep ?? defaultSleep;
  const concurrency = Math.max(1, Math.min(options.concurrency, BROWSER_CONCURRENCY_CAP));
  const heartbeatMs = options.heartbeatMs ?? HEARTBEAT_MS;
  const pollMs = options.pollMs ?? 2000;
  let needsBlobType = Boolean(options.azure);
  let skipsInARow = 0;

  async function uploadSingle(grant: Grant, file: Blob) {
    const { entry } = grant;
    let url = grant.upload_url;
    if (!url) url = (await api.uploadUrl(entry.position)).upload_url;
    if (!url) throw new Error("The API gave no upload URL for this entry.");

    await withRetry(
      async () => {
        const headers: Record<string, string> = { "Content-Type": entry.content_type ?? "application/octet-stream" };
        if (needsBlobType) headers["x-ms-blob-type"] = "BlockBlob";
        try {
          await put(url as string, file, {
            headers,
            signal,
            onProgress: (sent) => onEvent({ type: "progress", position: entry.position, sent }),
          });
        } catch (error) {
          if (error instanceof UploadHttpError) {
            // The grants do not say which provider this is, so Azure shows itself by refusing the PUT.
            if ((error.status === 400 || error.status === 409) && /x-ms-blob-type/i.test(error.body)) {
              needsBlobType = true;
            }
            if (error.urlExpired) {
              url = (await api.uploadUrl(entry.position)).upload_url ?? url;
            }
          }
          throw error;
        }
      },
      {
        signal,
        sleep: wait,
        shouldRetry: (error) =>
          isTransientPutError(error) ||
          (error instanceof UploadHttpError && (error.urlExpired || (needsBlobType && /x-ms-blob-type/i.test(error.body)))),
      },
    );
    return undefined;
  }

  async function uploadMultipart(grant: Grant, file: Blob) {
    const { entry } = grant;
    const parts = planParts(entry.size_bytes, choosePartSize(entry.size_bytes));
    const sent = new Map<number, number>();
    const finished: PartRecord[] = [];

    function report(partNumber: number, bytes: number) {
      sent.set(partNumber, bytes);
      let total = 0;
      for (const value of sent.values()) total += value;
      onEvent({ type: "progress", position: entry.position, sent: total });
    }

    for (let from = 0; from < parts.length; from += MAX_PART_BATCH) {
      const batch = parts.slice(from, from + MAX_PART_BATCH);
      const urls = new Map<number, string>();
      for (const item of (await api.partUrls(entry.position, batch.map((p) => p.number))).parts) {
        urls.set(item.part_number, item.url);
      }

      const records: PartRecord[] = [];
      await runPool(
        batch,
        PART_LANES,
        async (part) => {
          const result = await withRetry(
            async () => {
              try {
                return await put(urls.get(part.number) as string, file.slice(part.start, part.end), {
                  signal,
                  onProgress: (bytes) => report(part.number, bytes),
                });
              } catch (error) {
                if (error instanceof UploadHttpError && error.urlExpired) {
                  const fresh = await api.partUrls(entry.position, [part.number]);
                  urls.set(part.number, fresh.parts[0].url);
                }
                throw error;
              }
            },
            {
              signal,
              sleep: wait,
              shouldRetry: (error) => isTransientPutError(error) || (error instanceof UploadHttpError && error.urlExpired),
            },
          );
          // Azure block uploads expose no ETag and the server does not check one there.
          if (!result.etag && !options.azure) throw new MissingEtagError();
          records.push({ part_number: part.number, etag: result.etag ?? "", size_bytes: part.size });
        },
        signal,
      );

      records.sort((a, b) => a.part_number - b.part_number);
      await api.confirmParts(entry.position, records);
      finished.push(...records);
    }
    return finished.map((p) => ({ part_number: p.part_number, etag: p.etag }));
  }

  async function settle(entry: IngestionEntry, status: number) {
    let current = entry;
    let confirming = status === 202;
    for (let tries = 0; tries < 60; tries++) {
      if (!confirming && current.state !== "SCHEDULED" && current.state !== "UPLOADING") return current;
      await wait(pollMs, signal);
      current = await api.getEntry(entry.position);
      confirming = false;
    }
    return current;
  }

  async function handle(grant: Grant) {
    const { entry } = grant;
    const file = options.getFile(entry);
    if (!file) {
      skipsInARow++;
      onEvent({ type: "skipped", position: entry.position, message: "No attached file matches this path. Its slot frees itself when the lease expires." });
      return;
    }
    if (file.size !== entry.size_bytes) {
      skipsInARow++;
      onEvent({
        type: "skipped",
        position: entry.position,
        message: `The local file is ${file.size} bytes but the manifest says ${entry.size_bytes}. Not uploaded.`,
      });
      return;
    }

    skipsInARow = 0;
    onEvent({
      type: "started",
      position: entry.position,
      path: entry.path ?? "",
      size: entry.size_bytes,
      strategy: entry.strategy ?? "SINGLE",
    });
    const beat = setInterval(() => {
      api.heartbeat(entry.position).catch(() => undefined);
    }, heartbeatMs);

    try {
      const parts = entry.strategy === "MULTIPART" ? await uploadMultipart(grant, file) : await uploadSingle(grant, file);
      const { status, entry: after } = await api.complete(entry.position, parts);
      const final = await settle(after, status);
      if (final.state === "FAILED" || final.state === "FAILED_RETRYABLE") {
        onEvent({ type: "failed", position: entry.position, message: final.error_detail || final.error_code || "The server rejected the upload." });
      } else {
        onEvent({ type: "done", position: entry.position, state: final.state });
      }
    } catch (error) {
      if (signal.aborted) return;
      onEvent({ type: "failed", position: entry.position, message: messageOf(error) });
    } finally {
      clearInterval(beat);
    }
  }

  const active = new Set<Promise<void>>();
  let claimErrors = 0;

  while (!signal.aborted) {
    if (options.shouldPause()) {
      await Promise.all(active);
      return "paused";
    }
    if (skipsInARow >= MAX_SKIPS_IN_A_ROW) {
      await Promise.all(active);
      return "unmatched";
    }
    if (active.size >= concurrency) {
      await Promise.race(active);
      continue;
    }

    let result: ClaimResponse;
    try {
      result = await api.claim(Math.min(MAX_CLAIM, concurrency - active.size), signal);
      claimErrors = 0;
    } catch (error) {
      if (signal.aborted) break;
      const throttled = isApiError(error) && error.isRateLimited;
      const offline = isApiError(error) && error.isNetwork && ++claimErrors < 5;
      if (!throttled && !offline) throw error;
      const seconds = (isApiError(error) && error.retryAfter) || DEFAULT_WAIT_SECONDS;
      onEvent({ type: "waiting", seconds, reason: throttled ? "The API asked to slow down." : "The API could not be reached." });
      await wait(seconds * 1000, signal);
      continue;
    }

    const grants = result.grants ?? [];
    onEvent({ type: "claimed", count: grants.length, limitedBy: result.limited_by });
    for (const grant of grants) {
      const task = handle(grant).finally(() => active.delete(task));
      active.add(task);
    }
    if (grants.length > 0) continue;

    if (result.limited_by === "job_not_running") {
      await Promise.all(active);
      return "job_not_running";
    }
    if (result.limited_by === "no_pending_entries" && active.size === 0) return "finished";

    if (active.size > 0) {
      await Promise.race(active);
      continue;
    }
    const seconds = result.retry_after_seconds || DEFAULT_WAIT_SECONDS;
    onEvent({ type: "waiting", seconds, reason: result.limited_by ?? "nothing to claim" });
    await wait(seconds * 1000, signal);
  }

  await Promise.allSettled(active);
  return "paused";
}
