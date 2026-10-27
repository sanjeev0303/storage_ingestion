import { apiRaw, getActiveOrganization, newIdempotencyKey, type RequestOptions } from "@/lib/api/client";
import { ApiError, describeError, isApiError } from "@/lib/api/errors";
import { reportProblem } from "@/lib/errors/report";
import { choosePartSize, MIB, planParts, runPool, type Part } from "@/lib/upload/parts";
import { isTransientPutError, MissingEtagError, putBlob, UploadHttpError } from "@/lib/upload/put";
import { sleep as realSleep, withRetry } from "@/lib/upload/retry";
import { isAbort, sha256Blob } from "@/lib/upload/sha256";
import { formatBytes } from "@/lib/utils/format";
import type {
  FailureKind,
  MultipartStartResponse,
  PartUrlsResponse,
  ResumeRecord,
  SingleUploadResponse,
  StoredPart,
  UploadInfo,
  UploadRequest,
  UploadSnapshot,
  UploadStatus,
} from "../types";
import { memoryStore, type ResumeStore } from "./store";

export const SINGLE_LIMIT_BYTES = 16 * MIB;

type Putter = typeof putBlob;

export type EngineOptions = {
  put: Putter;
  hash: typeof sha256Blob;
  sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  store: ResumeStore;
  // How long to wait between status checks.
  pollMs: number;
  // How many status checks before the screen asks the person to check again.
  maxPolls: number;
  concurrency: number;
  // Parts are presigned and confirmed in groups of this size.
  partBatch: number;
  // False when the connection has no multipart support.
  hasMultipart: boolean;
  // Azure blocks come back without an ETag and do not need one.
  requireEtag: boolean;
};

const defaults: EngineOptions = {
  put: putBlob,
  hash: sha256Blob,
  sleep: realSleep,
  store: memoryStore(),
  pollMs: 3000,
  maxPolls: 40,
  concurrency: 4,
  partBatch: 16,
  hasMultipart: true,
  requireEtag: true,
};

type Call<T> = { status: number; data: T };

async function call<T>(path: string, options: RequestOptions): Promise<Call<T>> {
  const res = await apiRaw(path, options);
  const text = await res.text();
  return { status: res.status, data: (text ? JSON.parse(text) : undefined) as T };
}

export function classifyFailure(error: unknown): { kind: FailureKind; message: string } {
  if (error instanceof MissingEtagError) return { kind: "cors", message: error.message };

  if (error instanceof ApiError) {
    const body = (error.body ?? {}) as { limit?: string; max?: number };
    if (error.status === 409 && /not active/i.test(error.message)) {
      return {
        kind: "session_expired",
        message:
          "The upload session expired or was closed. Sessions last about 30 minutes. Start it again to upload the file from the beginning.",
      };
    }
    if (error.status === 413) {
      const limit = typeof body.max === "number" ? ` of ${formatBytes(body.max)}` : "";
      return { kind: "too_large", message: `This file is over the size limit${limit} for this organization.` };
    }
    if (error.status === 429 && error.code === "quota_exceeded") {
      return {
        kind: "quota",
        message: `Too many uploads are running for this organization (${body.limit ?? "quota"}). Wait for some to finish and try again.`,
      };
    }
    if (error.status === 422) {
      return {
        kind: "size_mismatch",
        message: "The stored size did not match the file. This upload cannot be recovered; start a new upload.",
      };
    }
    if (error.status === 403) return { kind: "forbidden", message: error.message };
  }

  if (error instanceof UploadHttpError) {
    const reference = reportProblem("upload", `provider answered ${error.status}: ${error.body}`);
    return {
      kind: "other",
      message: `The storage provider refused the upload. The connection may not allow writing here. Check the connection, then try again. Reference ${reference}.`,
    };
  }
  return { kind: "other", message: describeError(error) };
}

// How long to wait before trying an API call again, or null to give up.
function retryDelaySeconds(error: unknown, attempt: number): number | null {
  if (!(error instanceof ApiError)) return null;
  if (error.status === 429) return attempt < 6 ? (error.retryAfter ?? 5) : null;
  if (error.status === 0 || error.status === 502 || error.status === 503 || error.status === 504) {
    return attempt < 3 ? 2 ** (attempt + 1) : null;
  }
  if (error.status === 409 && /pending reconciliation|has not been uploaded/i.test(error.message)) {
    return attempt < 5 ? 3 : null;
  }
  return null;
}

const TERMINAL: UploadSnapshot["state"][] = ["available", "rejected", "failed", "cancelled"];

export class FileUpload {
  readonly file: File;
  private options: EngineOptions;
  private snap: UploadSnapshot;
  private listeners = new Set<() => void>();
  private controller = new AbortController();
  private key = newIdempotencyKey();
  private sha: string | undefined;
  private sentByPart = new Map<number, number>();
  private resumedBytes = 0;
  private samples: { at: number; bytes: number }[] = [];
  private lastEmit = 0;
  private record: ResumeRecord | undefined;
  private running = false;
  private watching: Promise<void> | undefined;

  constructor(file: File, request: UploadRequest, options: Partial<EngineOptions> = {}) {
    this.file = file;
    this.options = { ...defaults, ...options };
    this.snap = {
      id: crypto.randomUUID(),
      fileName: file.name,
      fileSize: file.size,
      request,
      state: "queued",
      hashedBytes: 0,
      sentBytes: 0,
      speedBps: 0,
      partsDone: 0,
      partsTotal: 0,
      pollingStopped: false,
      resumed: false,
    };
  }

  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = () => this.snap;

  get idempotencyKey() {
    return this.key;
  }

  get active() {
    return !TERMINAL.includes(this.snap.state) && this.snap.state !== "queued";
  }

  configure(options: Partial<EngineOptions>) {
    this.options = { ...this.options, ...options };
  }

  setRequest(request: UploadRequest) {
    if (this.snap.state === "queued") this.patch({ request });
  }

  async run(resume?: ResumeRecord) {
    if (this.snap.state !== "queued" || this.running) return;
    this.running = true;
    this.controller = new AbortController();
    // Out of "queued" at once: the file has its slot, and the screen says so.
    this.patch({ state: "hashing", hashedBytes: 0, failure: undefined, note: undefined, resumed: Boolean(resume) });

    try {
      if (resume) this.key = resume.idempotency_key;
      this.sha = resume?.sha256 || this.sha;

      const useMultipart = resume || (this.file.size > SINGLE_LIMIT_BYTES && this.options.hasMultipart);
      if (useMultipart) {
        // A large file is hashed while its parts upload, and the checksum
        // goes with the completion: reading it all first would add a whole
        // pass over the file before the first byte is sent.
        this.patch({ mode: "multipart" });
        await this.uploadMultipart(resume);
      } else {
        // A small one is hashed first: it takes well under a second, and the
        // single request declares it up front.
        if (!this.sha) {
          this.sha = await this.options.hash(this.file, {
            signal: this.signal,
            onProgress: (bytes) => this.throttled({ hashedBytes: bytes }),
          });
        }
        this.patch({ mode: "single" });
        await this.uploadSingle();
      }
    } catch (error) {
      await this.fail(error);
    } finally {
      this.running = false;
    }
  }

  // After a failure that can be repeated (network, provider refusal): keep the
  // same attempt, and for multipart pick up from the confirmed parts.
  async retry() {
    if (this.snap.state !== "failed") return;
    const saved = this.snap.uploadId ? await this.options.store.get(this.snap.uploadId) : undefined;
    this.patch({ state: "queued" });
    await this.run(saved);
  }

  // After the session expired or the size did not match: a brand new attempt.
  async restart() {
    if (this.snap.state !== "failed") return;
    this.key = newIdempotencyKey();
    this.record = undefined;
    this.sentByPart.clear();
    this.resumedBytes = 0;
    this.patch({ state: "queued", uploadId: undefined, sentBytes: 0, partsDone: 0, partsTotal: 0 });
    await this.run();
  }

  cancel() {
    if (TERMINAL.includes(this.snap.state)) return;
    const uploadId = this.snap.uploadId;
    const uploading = ["queued", "hashing", "uploading", "finalizing"].includes(this.snap.state);

    this.controller.abort();
    this.patch({ state: "cancelled", note: undefined });
    if (!uploadId || !uploading) return;

    // Best effort: a 409 here means the upload had already completed.
    call(`/uploads/${uploadId}/abort`, { method: "POST" }).catch(() => undefined);
    this.options.store.remove(uploadId);
  }

  // Stops status polling for an upload that is already stored.
  stopWatching() {
    this.controller.abort();
    this.patch({ pollingStopped: true });
  }

  async checkNow() {
    if (!["finalizing", "verifying", "processing"].includes(this.snap.state)) return;
    this.controller = new AbortController();
    this.patch({ pollingStopped: false, failure: undefined });
    try {
      const info = await this.uploadInfo();
      if (this.rejected(info)) return;
      await this.finish(info.status);
    } catch (error) {
      await this.fail(error);
    }
  }

  private get signal() {
    return this.controller.signal;
  }

  private patch(change: Partial<UploadSnapshot>) {
    if (this.snap.state === "cancelled" && change.state && change.state !== "cancelled") return;
    this.snap = { ...this.snap, ...change };
    for (const listener of this.listeners) listener();
  }

  // Progress arrives many times a second; the screen needs it a few times.
  private throttled(change: Partial<UploadSnapshot>) {
    const now = Date.now();
    if (now - this.lastEmit < 120) return;
    this.lastEmit = now;
    this.patch(change);
  }

  private sendProgress(force = false) {
    let sent = this.resumedBytes;
    for (const bytes of this.sentByPart.values()) sent += bytes;

    const now = Date.now();
    this.samples.push({ at: now, bytes: sent });
    this.samples = this.samples.filter((s) => now - s.at <= 5000);
    const first = this.samples[0];
    const speedBps = now > first.at ? ((sent - first.bytes) / (now - first.at)) * 1000 : 0;

    const change = { sentBytes: sent, speedBps: Math.max(speedBps, 0) };
    if (force) this.patch(change);
    else this.throttled(change);
  }

  private async request<T>(path: string, options: RequestOptions = {}): Promise<Call<T>> {
    for (let attempt = 0; ; attempt++) {
      try {
        return await call<T>(path, { ...options, signal: this.signal });
      } catch (error) {
        const wait = retryDelaySeconds(error, attempt);
        if (wait === null) throw error;
        this.patch({ note: `Waiting ${wait} seconds before trying again.` });
        await this.options.sleep(wait * 1000, this.signal);
        this.patch({ note: undefined });
      }
    }
  }

  private uploadInfo() {
    return this.request<UploadInfo>(`/uploads/${this.snap.uploadId}`).then((r) => r.data);
  }

  private async fail(error: unknown) {
    if (isAbort(error) || this.signal.aborted) return;
    const failure = classifyFailure(error);
    const uploadId = this.snap.uploadId;
    if (uploadId && (failure.kind === "session_expired" || failure.kind === "size_mismatch")) {
      await this.options.store.remove(uploadId);
    }
    this.patch({ state: "failed", failure, note: undefined, speedBps: 0 });
  }

  private body() {
    const { request } = this.snap;
    return {
      file_name: request.file_name,
      content_type: request.content_type,
      size_bytes: this.file.size,
      checksum_sha256: this.sha,
      idempotency_key: this.key,
      dataset: request.dataset || undefined,
      path: request.path || undefined,
      metadata: request.metadata && Object.keys(request.metadata).length > 0 ? request.metadata : undefined,
      create_only: request.create_only || undefined,
      storage_connection_id: request.storage_connection_id || undefined,
    };
  }

  private async uploadSingle() {
    this.patch({ state: "uploading", sentBytes: 0, partsTotal: 1, partsDone: 0 });
    const { request } = this.snap;

    let created = (await this.request<SingleUploadResponse>("/uploads", { method: "POST", body: this.body() })).data;
    this.patch({ uploadId: created.upload_id, storageConnectionId: created.storage_connection_id });

    await withRetry(
      async () => {
        try {
          // The URL signs the Content-Type, so it must match exactly.
          await this.options.put(created.upload_url, this.file, {
            headers: { ...created.upload_headers, "Content-Type": request.content_type },
            signal: this.signal,
            onProgress: (bytes) => {
              this.sentByPart.set(1, bytes);
              this.sendProgress();
            },
          });
        } catch (error) {
          if (error instanceof UploadHttpError && error.urlExpired) {
            // Same idempotency key: the API hands back the same session with a new URL.
            created = (await this.request<SingleUploadResponse>("/uploads", { method: "POST", body: this.body() })).data;
          }
          throw error;
        }
      },
      {
        signal: this.signal,
        sleep: this.options.sleep,
        shouldRetry: (error) => isTransientPutError(error) || (error instanceof UploadHttpError && error.urlExpired),
        onRetry: () => this.sentByPart.set(1, 0),
      },
    );
    this.sentByPart.set(1, this.file.size);
    this.patch({ partsDone: 1 });
    this.sendProgress(true);

    this.patch({ state: "finalizing" });
    const done = await this.request<{ status: UploadStatus }>(`/uploads/${created.upload_id}/complete`, { method: "POST" });
    this.watch(done.status === 202 ? "COMPLETING" : done.data.status);
  }

  private async uploadMultipart(resume?: ResumeRecord) {
    this.patch({ state: "uploading" });
    let record = resume;

    if (record) {
      this.patch({ uploadId: record.upload_id });
      const status = await this.checkResumable(record);
      if (status === "gone") {
        this.key = newIdempotencyKey();
        record = undefined;
        this.patch({
          resumed: false,
          uploadId: undefined,
          note: "The earlier upload session had expired or was closed, so this upload starts again from the beginning.",
        });
      } else if (status !== "open") {
        await this.options.store.remove(record.upload_id);
        this.watch(status);
        return;
      }
    }

    if (!record) {
      const start = await this.request<MultipartStartResponse>("/uploads/multipart", { method: "POST", body: this.body() });
      record = {
        upload_id: start.data.upload_id,
        organization_id: getActiveOrganization() ?? "",
        idempotency_key: this.key,
        request: this.snap.request,
        file_size: this.file.size,
        file_last_modified: this.file.lastModified,
        sha256: this.sha ?? "",
        part_size: choosePartSize(this.file.size),
        parts: [],
        expires_at: start.data.expires_at,
        saved_at: new Date().toISOString(),
      };
      this.patch({ uploadId: record.upload_id, storageConnectionId: start.data.storage_connection_id });
      await this.options.store.put(record);
    }
    this.record = record;

    // Runs alongside the parts and is only awaited at completion. A failure
    // is reported there, so it must not go unhandled before then.
    const hashing = this.sha ? Promise.resolve(this.sha) : this.hashWhileUploading();
    hashing.catch(() => undefined);

    const plan = planParts(this.file.size, record.part_size);
    const confirmed = new Map<number, StoredPart>(record.parts.map((p) => [p.number, p]));
    this.sentByPart.clear();
    this.resumedBytes = record.parts.reduce((sum, p) => sum + p.size, 0);
    this.patch({ partsTotal: plan.length, partsDone: confirmed.size });
    this.sendProgress(true);

    const missing = plan.filter((p) => !confirmed.has(p.number));
    for (let i = 0; i < missing.length; i += this.options.partBatch) {
      await this.uploadBatch(missing.slice(i, i + this.options.partBatch), confirmed);
    }

    this.patch({ state: "finalizing" });
    const checksum = await hashing;
    const parts = [...confirmed.values()]
      .sort((a, b) => a.number - b.number)
      .map((p) => ({ part_number: p.number, etag: p.etag }));
    // The server records this checksum, or checks it against the one the
    // upload started with, and verifies the stored bytes against it.
    const done = await this.request<{ status: UploadStatus }>(`/uploads/${record.upload_id}/complete`, {
      method: "POST",
      body: { parts, checksum_sha256: checksum },
    });
    await this.options.store.remove(record.upload_id);
    this.watch(done.status === 202 ? "COMPLETING" : done.data.status);
  }

  // Hashes the file while its parts upload. Progress has its own throttle so
  // it never crowds out the upload's own progress updates. The result is
  // saved with the resume record, so a resumed upload does not hash again.
  private async hashWhileUploading() {
    let lastEmit = 0;
    const sha = await this.options.hash(this.file, {
      signal: this.signal,
      onProgress: (bytes) => {
        const now = Date.now();
        if (now - lastEmit < 250) return;
        lastEmit = now;
        this.patch({ hashedBytes: bytes });
      },
    });
    this.sha = sha;
    this.patch({ hashedBytes: this.file.size });
    if (this.record) {
      this.record = { ...this.record, sha256: sha, saved_at: new Date().toISOString() };
      await this.options.store.put(this.record);
    }
    return sha;
  }

  // "open" means parts can still be sent; "gone" means start over.
  private async checkResumable(record: ResumeRecord): Promise<"open" | "gone" | UploadStatus> {
    try {
      const { data } = await this.request<UploadInfo>(`/uploads/${record.upload_id}`);
      const expired = Date.parse(data.expires_at) <= Date.now();
      if (data.status === "PENDING" || data.status === "UPLOADING") return expired ? "gone" : "open";
      if (data.status === "COMPLETING" || data.status === "COMPLETED" || data.status === "VERIFIED") return data.status;
      return "gone";
    } catch (error) {
      if (isApiError(error) && (error.isNotFound || error.isConflict)) return "gone";
      throw error;
    }
  }

  private async uploadBatch(batch: Part[], confirmed: Map<number, StoredPart>) {
    const urls = await this.presign(batch.map((p) => p.number));
    const finished: StoredPart[] = [];

    try {
      await runPool(
        batch,
        this.options.concurrency,
        async (part) => {
          const etag = await this.sendPart(part, urls);
          finished.push({ number: part.number, etag, size: part.size });
        },
        this.signal,
      );
    } catch (error) {
      // Keep the ETags of parts that did go through; they are lost with the page otherwise.
      if (finished.length > 0 && !this.signal.aborted) await this.confirm(finished, confirmed).catch(() => undefined);
      throw error;
    }
    this.signal.throwIfAborted();
    await this.confirm(finished, confirmed);
  }

  private async confirm(finished: StoredPart[], confirmed: Map<number, StoredPart>) {
    const parts = [...finished]
      .sort((a, b) => a.number - b.number)
      .map((p) => ({ part_number: p.number, etag: p.etag, size_bytes: p.size }));
    await this.request(`/uploads/${this.snap.uploadId}/parts/confirm`, { method: "POST", body: { parts } });

    for (const part of finished) confirmed.set(part.number, part);
    this.patch({ partsDone: confirmed.size });
    if (this.record) {
      this.record = { ...this.record, parts: [...confirmed.values()], saved_at: new Date().toISOString() };
      await this.options.store.put(this.record);
    }
  }

  private async presign(numbers: number[]) {
    const { data } = await this.request<PartUrlsResponse>(`/uploads/${this.snap.uploadId}/parts/urls`, {
      method: "POST",
      body: { part_numbers: numbers },
    });
    return new Map(data.parts.map((p) => [p.part_number, p.url]));
  }

  private async sendPart(part: Part, urls: Map<number, string>) {
    const blob = this.file.slice(part.start, part.end);

    const result = await withRetry(
      async () => {
        try {
          return await this.options.put(urls.get(part.number)!, blob, {
            signal: this.signal,
            onProgress: (bytes) => {
              this.sentByPart.set(part.number, bytes);
              this.sendProgress();
            },
          });
        } catch (error) {
          if (error instanceof UploadHttpError && error.urlExpired) {
            // Only this part gets a new URL; the rest of the upload carries on.
            const fresh = await this.presign([part.number]);
            urls.set(part.number, fresh.get(part.number)!);
          }
          throw error;
        }
      },
      {
        signal: this.signal,
        sleep: this.options.sleep,
        shouldRetry: (error) => isTransientPutError(error) || (error instanceof UploadHttpError && error.urlExpired),
        onRetry: () => this.sentByPart.set(part.number, 0),
      },
    );

    if (!result.etag && this.options.requireEtag) throw new MissingEtagError();
    this.sentByPart.set(part.number, part.size);
    this.sendProgress(true);
    return result.etag ?? "";
  }

  // The bytes are stored: the rest is the server's work. Watching it does
  // not hold the run() caller, so the queue can start the next file.
  private watch(status: UploadStatus) {
    this.watching = this.finish(status).catch((error) => this.fail(error));
  }

  // Resolves once the server's verdict on the stored bytes is known (or
  // watching stopped).
  settled() {
    return this.watching ?? Promise.resolve();
  }

  // From here the bytes are stored. The API then verifies, scans and
  // publishes them, and the upload's status reports each step.
  private async finish(status: UploadStatus) {
    const verified = await this.waitForVerified(status);
    if (!verified) return;
    await this.watchAsset();
  }

  // Processing usually takes well under a second for small files, so the
  // first checks come quickly and then slow down to pollMs.
  private pollDelay(polls: number) {
    return Math.min(this.options.pollMs, 500 * 2 ** polls);
  }

  // A rejected upload never gets a catalog version; the upload itself says so.
  private rejected(info: UploadInfo) {
    if (info.asset?.status !== "REJECTED") return false;
    this.patch({ state: "rejected", asset: { ...this.snap.asset, status: "REJECTED", rejectionReason: info.asset.rejection_reason } });
    return true;
  }

  private async waitForVerified(first: UploadStatus) {
    let status = first;
    for (let polls = 0; ; polls++) {
      if (status === "VERIFIED") return true;
      if (status === "FAILED" || status === "ABORTED" || status === "EXPIRED") {
        if (this.snap.uploadId) await this.options.store.remove(this.snap.uploadId);
        this.patch({
          state: "failed",
          failure: { kind: "other", message: `The server ended the upload with status ${status}. Start a new upload.` },
        });
        return false;
      }

      this.patch({ state: status === "COMPLETING" ? "finalizing" : "verifying" });
      if (polls >= this.options.maxPolls) {
        this.patch({ pollingStopped: true });
        return false;
      }
      await this.options.sleep(this.pollDelay(polls), this.signal);
      const info = await this.uploadInfo();
      if (this.rejected(info)) return false;
      status = info.status;
    }
  }

  // Polls the upload alone: it reports what the upload became and, once
  // published, its catalog object. One request a check, however many files
  // are waiting, so a large batch does not crowd out the uploads still going.
  private async watchAsset() {
    this.patch({ state: "processing", speedBps: 0 });

    for (let polls = 0; ; polls++) {
      const info = await this.uploadInfo();
      if (this.rejected(info)) return;
      const asset = info.asset;
      if (asset) {
        this.patch({ asset: { objectId: asset.object_id || undefined, status: asset.status } });
        if (asset.status === "AVAILABLE") {
          this.patch({ state: "available" });
          return;
        }
      }

      if (polls >= this.options.maxPolls) {
        this.patch({ pollingStopped: true });
        return;
      }
      await this.options.sleep(this.pollDelay(polls), this.signal);
    }
  }
}
