import { apiRaw, getActiveOrganization, newIdempotencyKey, type RequestOptions } from "@/lib/api/client";
import { ApiError, describeError, isApiError } from "@/lib/api/errors";
import { reportProblem } from "@/lib/errors/report";
import { choosePartSize, MIB, planParts, type Part } from "@/lib/upload/parts";
import { isTransientPutError, MissingEtagError, putBlob, UploadHttpError, UploadNetworkError } from "@/lib/upload/put";
import { sleep as realSleep } from "@/lib/upload/retry";
import { abortError, isAbort, sha256Blob } from "@/lib/upload/sha256";
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
import {
  backoffMs,
  isNetworkFailure,
  preferredPartBytes,
  sharedConnection,
  SLOW_BPS,
  StalledError,
  type ConnectionWatch,
} from "./network";
import { memoryStore, type ResumeStore } from "./store";

export const SINGLE_LIMIT_BYTES = 16 * MIB;
// The start of a file, hashed to tell the same file when it is chosen again.
export const HEAD_BYTES = 1 * MIB;

// Attempts for one PUT or API call before the upload fails.
const PUT_ATTEMPTS = 6;
const API_ATTEMPTS = 6;
// Parts in flight: never fewer than one, and never more than the six
// connections a browser opens to one host.
const MIN_LANES = 1;
const MAX_LANES = 6;
const SPEED_WINDOW_MS = 8000;

type Putter = typeof putBlob;

export type EngineOptions = {
  put: Putter;
  hash: typeof sha256Blob;
  sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  store: ResumeStore;
  connection: ConnectionWatch;
  // How long to wait between status checks.
  pollMs: number;
  // How many status checks before the screen asks the person to check again.
  maxPolls: number;
  // Parts in flight at the start; it then follows the measured speed.
  concurrency: number;
  // Parts are presigned and confirmed in groups of this size.
  partBatch: number;
  // A PUT that sends nothing for this long is stopped and tried again.
  stallMs: number;
  random: () => number;
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
  connection: sharedConnection(),
  pollMs: 3000,
  maxPolls: 40,
  concurrency: 4,
  partBatch: 16,
  stallMs: 20_000,
  random: Math.random,
  hasMultipart: true,
  requireEtag: true,
};

type Call<T> = { status: number; data: T };

async function call<T>(path: string, options: RequestOptions): Promise<Call<T>> {
  const res = await apiRaw(path, options);
  const text = await res.text();
  return { status: res.status, data: (text ? JSON.parse(text) : undefined) as T };
}

export function headSha256(file: Blob, hash: (blob: Blob) => Promise<string> = sha256Blob) {
  return hash(file.slice(0, HEAD_BYTES));
}

// Whether a file chosen again is the one the record was made for: the same
// name, size and first MiB. Records saved before the hash was kept compare
// the last modified time instead.
export async function isSameFile(file: File, record: ResumeRecord, hash: (blob: Blob) => Promise<string> = sha256Blob) {
  if (file.name !== record.request.file_name || file.size !== record.file_size) return false;
  if (!record.head_sha256) return file.lastModified === record.file_last_modified;
  return (await headSha256(file, hash)) === record.head_sha256;
}

export function classifyFailure(error: unknown): { kind: FailureKind; message: string } {
  if (error instanceof MissingEtagError) return { kind: "cors", message: error.message };
  if (error instanceof StalledError) {
    return {
      kind: "other",
      message: "Nothing reached the storage provider for 20 seconds, several times in a row. Check the connection, then retry.",
    };
  }
  if (error instanceof UploadNetworkError) {
    return { kind: "other", message: "The storage provider could not be reached. Check the connection, then retry." };
  }

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
// Only answers that can change are retried; Retry-After wins when given.
function apiRetryDelayMs(error: unknown, attempt: number, random: () => number): number | null {
  if (!(error instanceof ApiError) || attempt >= API_ATTEMPTS - 1) return null;
  if (error.status === 409 && /pending reconciliation|has not been uploaded/i.test(error.message)) return 3000;
  const retryable = error.status === 0 || error.status === 408 || error.status === 429 || error.status >= 500;
  if (!retryable) return null;
  if (error.retryAfter) return error.retryAfter * 1000;
  return backoffMs(attempt, random);
}

const TERMINAL: UploadSnapshot["state"][] = ["available", "rejected", "failed", "cancelled"];
// States that wait on the network, and so pause when it goes.
const NETWORKED: UploadSnapshot["state"][] = ["uploading", "finalizing", "verifying", "processing"];

type Target = { url: string; headers?: Record<string, string> };

export class FileUpload {
  readonly file: File;
  private options: EngineOptions;
  private snap: UploadSnapshot;
  private listeners = new Set<() => void>();
  // Lives as long as the attempt: cancelling aborts it.
  private controller = new AbortController();
  // One stretch of network work: a pause aborts it, going on starts a new one.
  private leg = new AbortController();
  private wakers = new Set<() => void>();
  private unfollow: (() => void) | undefined;
  private key = newIdempotencyKey();
  private sha: string | undefined;
  private sentByPart = new Map<number, number>();
  private resumedBytes = 0;
  private samples: { at: number; bytes: number }[] = [];
  private lastEmit = 0;
  private lanes: number;
  private record: ResumeRecord | undefined;
  private confirmed = new Map<number, StoredPart>();
  private unconfirmed = new Map<number, StoredPart>();
  private saving = Promise.resolve();
  private running = false;
  private watching: Promise<void> | undefined;
  // A retry or restart on its way; a second press joins it.
  private again: Promise<void> | undefined;
  // The organization the upload started in. Every request names it, so
  // switching organization meanwhile does not move the upload.
  private org: string | undefined;
  private stopped = false;
  // A single upload's completion was sent under the current key. Unlike
  // snap.completionSent, a retry does not clear it.
  private sentCompletion = false;

  constructor(file: File, request: UploadRequest, options: Partial<EngineOptions> = {}) {
    this.file = file;
    this.options = { ...defaults, ...options };
    this.lanes = this.options.concurrency;
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
    if (this.snap.state !== "queued" || this.running || this.stopped) return;
    this.running = true;
    this.org ??= resume?.organization_id || getActiveOrganization() || undefined;
    this.controller = new AbortController();
    this.leg = new AbortController();
    this.follow();
    // Out of "queued" at once: the file has its slot, and the screen says so.
    this.patch({ state: "hashing", hashedBytes: 0, failure: undefined, note: undefined, resumed: Boolean(resume), completionSent: false });

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
            signal: this.controller.signal,
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
  retry() {
    this.again ??= this.retryOnce().finally(() => {
      this.again = undefined;
    });
    return this.again;
  }

  // After the session expired or the size did not match: a brand new attempt.
  restart() {
    this.again ??= this.restartOnce().finally(() => {
      this.again = undefined;
    });
    return this.again;
  }

  private async retryOnce() {
    if (this.snap.state !== "failed") return;
    const saved = this.snap.uploadId ? await this.options.store.get(this.snap.uploadId) : undefined;
    this.patch({ state: "queued" });
    await this.run(saved);
  }

  private async restartOnce() {
    if (this.snap.state !== "failed") return;
    // Let a save from the failed attempt land first, so it cannot write the
    // old record back after it is let go.
    await this.saving;
    this.key = newIdempotencyKey();
    this.sentCompletion = false;
    this.record = undefined;
    this.confirmed.clear();
    this.unconfirmed.clear();
    this.sentByPart.clear();
    this.resumedBytes = 0;
    this.patch({ state: "queued", uploadId: undefined, sentBytes: 0, partsDone: 0, partsTotal: 0 });
    await this.run();
  }

  // Stops the parts in flight. Parts already sent are kept; resume() sends the rest.
  pause() {
    if (this.snap.state !== "uploading" || this.snap.paused === "user") return;
    this.pauseFor("user");
  }

  async resume() {
    const paused = this.snap.paused;
    if (!paused) return;
    const connection = this.options.connection;
    if (paused === "offline") {
      // Check now rather than wait for the next scheduled check.
      await connection.recheck();
      return;
    }
    if (connection.offline) this.patch({ paused: "offline" });
    else this.unpause();
  }

  cancel() {
    // Once the completion is sent the server may publish the file whatever
    // the console does, so the upload can no longer honestly say Cancelled.
    if (TERMINAL.includes(this.snap.state) || this.snap.completionSent) return;
    const uploadId = this.snap.uploadId;
    const uploading = ["queued", "hashing", "uploading", "finalizing"].includes(this.snap.state);

    this.controller.abort();
    this.leg.abort();
    this.patch({ state: "cancelled", note: undefined, paused: undefined });
    if (!uploadId || !uploading) return;

    // Best effort: a 409 here means the session had already ended.
    call(`/uploads/${uploadId}/abort`, { method: "POST", org: this.org }).catch(() => undefined);
    // After any save still on its way, or that save would bring the record back.
    void this.saving.then(() => this.options.store.remove(uploadId));
  }

  // Stops status polling for an upload that is already stored.
  stopWatching() {
    this.controller.abort();
    this.leg.abort();
    this.patch({ pollingStopped: true, paused: undefined });
  }

  // Leaving the page stops the upload as closing the tab would: nothing is
  // aborted on the server, and a multipart upload keeps its saved record, so
  // it can be continued from Unfinished uploads.
  stop() {
    this.stopped = true;
    this.controller.abort();
    this.leg.abort();
    this.unfollow?.();
    this.unfollow = undefined;
    this.options.connection.forgetSpeed(this);
  }

  async checkNow() {
    // Only once checking stopped: otherwise a second loop would run next to the live one.
    if (!this.snap.pollingStopped || !["finalizing", "verifying", "processing"].includes(this.snap.state)) return;
    this.controller = new AbortController();
    this.leg = new AbortController();
    this.follow();
    this.patch({ pollingStopped: false, failure: undefined });
    try {
      const info = await this.uploadInfo();
      if (this.rejected(info)) return;
      await this.finish(info.status);
    } catch (error) {
      await this.fail(error);
    }
  }

  // Network requests use this; a pause aborts it.
  private get signal() {
    return this.leg.signal;
  }

  private patch(change: Partial<UploadSnapshot>) {
    if (this.snap.state === "cancelled" && change.state && change.state !== "cancelled") return;
    this.snap = { ...this.snap, ...change };
    if (TERMINAL.includes(this.snap.state)) {
      // A finished upload waits on nothing, so it cannot stay paused.
      this.snap.paused = undefined;
      this.unfollow?.();
      this.unfollow = undefined;
    }
    if (this.snap.state !== "uploading" || this.snap.paused) this.options.connection.forgetSpeed(this);
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
    // One sample from before the window stays, so a slow link that reports
    // progress rarely still has a span to measure.
    while (this.samples.length > 2 && now - this.samples[1].at > SPEED_WINDOW_MS) this.samples.shift();
    const first = this.samples[0];
    const span = now - first.at;
    const speedBps = span > 0 ? Math.max(((sent - first.bytes) / span) * 1000, 0) : 0;
    // A few seconds of samples first, so the start of a part is not called slow.
    const measured = span >= 3000;
    if (measured) this.options.connection.reportSpeed(this, speedBps);

    // Speed and time left are shown once measured; the first moments say little.
    const change = { sentBytes: sent, speedBps: measured ? speedBps : 0, slow: measured && speedBps < SLOW_BPS };
    if (force) this.patch(change);
    else this.throttled(change);
  }

  // Aim for parts that take under a minute each. On a slow link, parts in
  // parallel only split the same bandwidth: each one takes longer, is likelier
  // to stall, and more half-sent bytes are lost when the connection drops.
  private adjustLanes(partSize: number) {
    const speed = this.snap.speedBps;
    if (speed <= 0) return;
    const secondsPerPart = (partSize * this.lanes) / speed;
    if (secondsPerPart > 60 && this.lanes > MIN_LANES) this.lanes--;
    else if (secondsPerPart < 15 && this.lanes < MAX_LANES) this.lanes++;
  }

  // Watches the connection while there is network work to do.
  private follow() {
    this.unfollow ??= this.options.connection.subscribe(this.onConnectionChange);
  }

  private onConnectionChange = () => {
    if (this.options.connection.offline) {
      if (!this.snap.paused && !this.snap.pollingStopped && NETWORKED.includes(this.snap.state)) this.pauseFor("offline");
    } else if (this.snap.paused === "offline") {
      this.unpause();
    }
  };

  // Half-sent parts are lost when their requests stop, so only finished
  // parts count as kept.
  private pauseFor(reason: "user" | "offline") {
    this.leg.abort();
    if (this.snap.mode === "multipart") {
      this.sentByPart.clear();
      this.resumedBytes = 0;
      for (const part of [...this.confirmed.values(), ...this.unconfirmed.values()]) this.resumedBytes += part.size;
    } else {
      this.sentByPart.set(1, 0);
    }
    this.patch({ paused: reason, sentBytes: this.resumedBytes, speedBps: 0, slow: false, note: undefined });
  }

  private unpause() {
    this.leg = new AbortController();
    this.samples = [];
    this.patch({ paused: undefined });
    for (const wake of this.wakers) wake();
    this.wakers.clear();
  }

  private async whilePaused() {
    while (this.snap.paused) {
      const signal = this.controller.signal;
      await new Promise<void>((resolve, reject) => {
        if (signal.aborted) return reject(abortError());
        const onAbort = () => reject(abortError());
        signal.addEventListener("abort", onAbort, { once: true });
        this.wakers.add(() => {
          signal.removeEventListener("abort", onAbort);
          resolve();
        });
      });
    }
  }

  // A request could not get through. Resolves true when the API cannot be
  // reached either: the upload is then paused until it can.
  private async lostConnection() {
    const offline = await this.options.connection.reportFailure();
    if (offline && !this.snap.paused) this.pauseFor("offline");
    return offline;
  }

  // Runs a step until it finishes. A pause stops the step part way; it runs
  // again once the upload goes on, and skips the work already done.
  private async keepGoing<T>(step: () => Promise<T>): Promise<T> {
    for (;;) {
      try {
        return await step();
      } catch (error) {
        if (this.controller.signal.aborted) throw error;
        // An abort that did not come from cancelling came from a pause.
        const paused = Boolean(this.snap.paused) || isAbort(error) || (isNetworkFailure(error) && (await this.lostConnection()));
        if (!paused) throw error;
        await this.whilePaused();
      }
    }
  }

  private async wait(ms: number) {
    if (ms >= 1000) this.patch({ note: `Waiting ${Math.round(ms / 1000)} seconds before trying again.` });
    await this.options.sleep(ms, this.signal);
    this.patch({ note: undefined });
  }

  private async request<T>(path: string, options: RequestOptions = {}): Promise<Call<T>> {
    for (let attempt = 0; ; attempt++) {
      try {
        return await call<T>(path, { ...options, org: this.org, signal: this.signal });
      } catch (error) {
        // Offline: stop here rather than use up the retries; keepGoing waits.
        if (isNetworkFailure(error) && (await this.lostConnection())) throw error;
        const wait = apiRetryDelayMs(error, attempt, this.options.random);
        if (wait === null) throw error;
        await this.wait(wait);
      }
    }
  }

  private uploadInfo() {
    return this.keepGoing(() => this.request<UploadInfo>(`/uploads/${this.snap.uploadId}`)).then((r) => r.data);
  }

  private async fail(error: unknown) {
    if (isAbort(error) || this.controller.signal.aborted) return;
    const failure = classifyFailure(error);
    const uploadId = this.snap.uploadId;
    if (uploadId && (failure.kind === "session_expired" || failure.kind === "size_mismatch")) {
      // Let go of the record first: a checksum still being read would save it
      // again when it finishes.
      this.record = undefined;
      await this.saving;
      await this.options.store.remove(uploadId);
    }
    this.patch({ state: "failed", failure, note: undefined, speedBps: 0, paused: undefined, slow: false });
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

  // Watches a PUT for progress instead of timing it as a whole: a slow link
  // may need many minutes for a part, but one that sends nothing for stallMs
  // is stuck, and is stopped so the part can go again.
  private async putWatched(blob: Blob, target: Target, onProgress: (bytes: number) => void) {
    const stop = new AbortController();
    let stalled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const arm = () => {
      clearTimeout(timer);
      timer = setTimeout(() => {
        stalled = true;
        stop.abort();
      }, this.options.stallMs);
    };

    arm();
    try {
      return await this.options.put(target.url, blob, {
        headers: target.headers,
        signal: AbortSignal.any([this.signal, stop.signal]),
        timeoutMs: 0,
        onProgress: (bytes) => {
          arm();
          onProgress(bytes);
        },
      });
    } catch (error) {
      if (stalled && !this.signal.aborted) throw new StalledError();
      throw error;
    } finally {
      clearTimeout(timer);
    }
  }

  // Sends a blob to a presigned URL, retrying what can pass: a network error
  // or stall while the API still answers, 408, 429, 5xx, and an expired URL,
  // which refresh() replaces. Anything else would fail the same way again.
  private async putWithRetries(blob: Blob, target: () => Target, refresh: () => Promise<void>, onProgress: (bytes: number) => void) {
    for (let attempt = 0; ; attempt++) {
      try {
        return await this.putWatched(blob, target(), onProgress);
      } catch (error) {
        onProgress(0);
        if (isAbort(error) || this.signal.aborted) throw error;
        const expired = error instanceof UploadHttpError && error.urlExpired;
        const stalled = error instanceof StalledError;
        if (!expired && !stalled && !isTransientPutError(error)) throw error;
        if (attempt >= PUT_ATTEMPTS - 1) throw error;
        if (stalled) this.lanes = Math.max(MIN_LANES, this.lanes - 1);
        if (isNetworkFailure(error) && (await this.lostConnection())) throw error;
        if (expired) {
          await refresh();
          continue;
        }
        await this.wait(backoffMs(attempt, this.options.random));
      }
    }
  }

  private async uploadSingle() {
    this.patch({ state: "uploading", sentBytes: 0, partsTotal: 1, partsDone: 0 });
    const { request } = this.snap;
    const create = async () => {
      try {
        return await this.request<SingleUploadResponse>("/uploads", { method: "POST", body: this.body() });
      } catch (error) {
        // A retry repeats the key, and the session it names has ended before
        // its bytes arrived. Start a new one, as a multipart upload does when
        // its session is gone. Not once a completion was sent: that session
        // may still become the stored file.
        if (!(isApiError(error) && error.code === "session_ended") || this.sentCompletion) throw error;
        this.key = newIdempotencyKey();
        this.patch({ note: "The earlier upload session had expired or was closed, so this upload starts again from the beginning." });
        return this.request<SingleUploadResponse>("/uploads", { method: "POST", body: this.body() });
      }
    };

    let created = (await this.keepGoing(create)).data;
    this.patch({ uploadId: created.upload_id, storageConnectionId: created.storage_connection_id });

    // A retry after a completion that went through: the file is stored already.
    if (!created.upload_url) {
      this.patch({ completionSent: true, partsDone: 1, sentBytes: this.file.size });
      this.watch(created.status);
      return;
    }

    await this.keepGoing(() =>
      this.putWithRetries(
        this.file,
        // The URL signs the Content-Type, so it must match exactly.
        () => ({ url: created.upload_url, headers: { ...created.upload_headers, "Content-Type": request.content_type } }),
        // Same idempotency key: the API hands back the same session with a new URL.
        async () => {
          created = (await create()).data;
          this.patch({ uploadId: created.upload_id });
        },
        (bytes) => {
          this.sentByPart.set(1, bytes);
          this.sendProgress();
        },
      ),
    );
    this.sentByPart.set(1, this.file.size);
    this.patch({ partsDone: 1 });
    this.sendProgress(true);

    this.patch({ state: "finalizing", completionSent: true });
    this.sentCompletion = true;
    const done = await this.keepGoing(() =>
      this.request<{ status: UploadStatus }>(`/uploads/${created.upload_id}/complete`, { method: "POST" }),
    );
    this.watch(done.status === 202 ? "COMPLETING" : done.data.status);
  }

  private async uploadMultipart(resume?: ResumeRecord) {
    this.patch({ state: "uploading" });
    let record = resume;

    if (record) {
      const saved = record;
      this.patch({ uploadId: saved.upload_id });
      const status = await this.keepGoing(() => this.checkResumable(saved));
      if (status === "gone") {
        this.key = newIdempotencyKey();
        record = undefined;
        this.patch({
          resumed: false,
          uploadId: undefined,
          note: "The earlier upload session had expired or was closed, so this upload starts again from the beginning.",
        });
      } else if (status !== "open") {
        this.patch({ completionSent: true });
        await this.options.store.remove(saved.upload_id);
        this.watch(status);
        return;
      }
    }

    if (!record) {
      const head = await headSha256(this.file, (blob) => this.options.hash(blob, { signal: this.controller.signal }));
      const start = await this.keepGoing(() =>
        this.request<MultipartStartResponse>("/uploads/multipart", { method: "POST", body: this.body() }),
      );
      record = {
        upload_id: start.data.upload_id,
        organization_id: this.org ?? "",
        idempotency_key: this.key,
        request: this.snap.request,
        file_size: this.file.size,
        file_last_modified: this.file.lastModified,
        sha256: this.sha ?? "",
        head_sha256: head,
        part_size: choosePartSize(this.file.size, preferredPartBytes(this.options.connection.measuredBps)),
        parts: [],
        unconfirmed: [],
        expires_at: start.data.expires_at,
        saved_at: new Date().toISOString(),
      };
      this.patch({ uploadId: record.upload_id, storageConnectionId: start.data.storage_connection_id });
      await this.options.store.put(record);
    }
    this.record = record;
    this.confirmed = new Map(record.parts.map((p) => [p.number, p]));
    this.unconfirmed = new Map((record.unconfirmed ?? []).map((p) => [p.number, p]));

    // Runs alongside the parts and is only awaited at completion. A failure
    // is reported there, so it must not go unhandled before then.
    const hashing = this.sha ? Promise.resolve(this.sha) : this.hashWhileUploading();
    hashing.catch(() => undefined);

    const plan = planParts(this.file.size, record.part_size);
    this.patch({ partsTotal: plan.length, partsDone: this.confirmed.size });
    await this.keepGoing(() => this.sendMissingParts(plan));

    this.patch({ state: "finalizing" });
    const checksum = await hashing;
    const parts = [...this.confirmed.values()]
      .sort((a, b) => a.number - b.number)
      .map((p) => ({ part_number: p.number, etag: p.etag }));
    // The server records this checksum, or checks it against the one the
    // upload started with, and verifies the stored bytes against it.
    const uploadId = record.upload_id;
    this.patch({ completionSent: true });
    const done = await this.keepGoing(() =>
      this.request<{ status: UploadStatus }>(`/uploads/${uploadId}/complete`, {
        method: "POST",
        body: { parts, checksum_sha256: checksum },
      }),
    );
    await this.saving;
    await this.options.store.remove(uploadId);
    this.watch(done.status === 202 ? "COMPLETING" : done.data.status);
  }

  // Hashes the file while its parts upload. Progress has its own throttle so
  // it never crowds out the upload's own progress updates. The result is
  // saved with the resume record, so a resumed upload does not hash again.
  // It runs on the attempt's signal, so a pause does not stop it.
  private async hashWhileUploading() {
    let lastEmit = 0;
    const sha = await this.options.hash(this.file, {
      signal: this.controller.signal,
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
      this.record = { ...this.record, sha256: sha };
      await this.save();
    }
    return sha;
  }

  // Saves in order, so two parts finishing together cannot leave the older
  // record on disk.
  private save() {
    if (!this.record) return this.saving;
    const record: ResumeRecord = {
      ...this.record,
      parts: [...this.confirmed.values()],
      unconfirmed: [...this.unconfirmed.values()],
      saved_at: new Date().toISOString(),
    };
    this.record = record;
    this.saving = this.saving.then(() => this.options.store.put(record));
    return this.saving;
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

  // Runs again after every pause. Parts confirmed or already sent are skipped.
  private async sendMissingParts(plan: Part[]) {
    if (this.unconfirmed.size > 0) await this.confirmLeftovers();

    this.sentByPart.clear();
    this.resumedBytes = 0;
    for (const part of [...this.confirmed.values(), ...this.unconfirmed.values()]) this.resumedBytes += part.size;
    this.sendProgress(true);

    const missing = plan.filter((p) => !this.confirmed.has(p.number) && !this.unconfirmed.has(p.number));
    for (let i = 0; i < missing.length; i += this.options.partBatch) {
      await this.uploadBatch(missing.slice(i, i + this.options.partBatch));
    }
  }

  // Parts sent before the connection dropped or the tab closed. The API checks
  // each one against the provider's own list; if it refuses them, they go again.
  private async confirmLeftovers() {
    try {
      await this.confirm([...this.unconfirmed.values()]);
    } catch (error) {
      if (!(isApiError(error) && error.status === 400)) throw error;
      this.unconfirmed.clear();
      await this.save();
    }
  }

  private async uploadBatch(batch: Part[]) {
    const urls = await this.presign(batch.map((p) => p.number));
    await this.sendAll(batch, async (part) => {
      const etag = await this.sendPart(part, urls);
      // Kept at once: if the page closes before the batch is confirmed, the
      // part is confirmed on resume instead of being sent again.
      this.unconfirmed.set(part.number, { number: part.number, etag, size: part.size });
      await this.save();
    });
    await this.confirm([...this.unconfirmed.values()]);
  }

  // Like runPool, except the number of parts in flight follows this.lanes,
  // which changes as the speed is measured.
  private async sendAll(parts: Part[], send: (part: Part) => Promise<void>) {
    const running = new Set<Promise<void>>();
    let failure: { error: unknown } | undefined;

    for (const part of parts) {
      while (running.size >= this.lanes) await Promise.race(running);
      if (failure || this.signal.aborted) break;
      const job: Promise<void> = send(part)
        .catch((error) => {
          failure ??= { error };
        })
        .finally(() => running.delete(job));
      running.add(job);
    }
    await Promise.all(running);
    if (failure) throw failure.error;
    this.signal.throwIfAborted();
  }

  private async confirm(parts: StoredPart[]) {
    if (parts.length === 0) return;
    const body = {
      parts: [...parts]
        .sort((a, b) => a.number - b.number)
        .map((p) => ({ part_number: p.number, etag: p.etag, size_bytes: p.size })),
    };
    await this.request(`/uploads/${this.snap.uploadId}/parts/confirm`, { method: "POST", body });

    for (const part of parts) {
      this.confirmed.set(part.number, part);
      this.unconfirmed.delete(part.number);
    }
    this.patch({ partsDone: this.confirmed.size });
    await this.save();
  }

  private async presign(numbers: number[]) {
    const { data } = await this.request<PartUrlsResponse>(`/uploads/${this.snap.uploadId}/parts/urls`, {
      method: "POST",
      body: { part_numbers: numbers },
    });
    return new Map(data.parts.map((p) => [p.part_number, p.url]));
  }

  private async sendPart(part: Part, urls: Map<number, string>) {
    const result = await this.putWithRetries(
      this.file.slice(part.start, part.end),
      () => ({ url: urls.get(part.number)! }),
      // Only this part gets a new URL; the rest of the upload carries on.
      async () => {
        const fresh = await this.presign([part.number]);
        urls.set(part.number, fresh.get(part.number)!);
      },
      (bytes) => {
        this.sentByPart.set(part.number, bytes);
        this.sendProgress();
      },
    );

    if (!result.etag && this.options.requireEtag) throw new MissingEtagError();
    this.sentByPart.set(part.number, part.size);
    this.sendProgress(true);
    this.adjustLanes(part.size);
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
      await this.options.sleep(this.pollDelay(polls), this.controller.signal);
      const info = await this.uploadInfo();
      if (this.rejected(info)) return false;
      status = info.status;
    }
  }

  // Polls the upload alone: it reports what the upload became and, once
  // published, its catalog object. One request a check, however many files
  // are waiting, so a large batch does not crowd out the uploads still going.
  private async watchAsset() {
    this.patch({ state: "processing", speedBps: 0, slow: false });

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
      await this.options.sleep(this.pollDelay(polls), this.controller.signal);
    }
  }
}
