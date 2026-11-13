// Resumable, hash-verified uploads from the browser and Node (docs/sdks.md).
//
//   const client = new Client({ baseUrl, token });
//   const asset = await upload(client, file, { dataset: "scans", path: "2026/a.dcm" });
//
// Parts go straight to storage with presigned URLs, so a proxy's limit on
// request bodies (Cloudflare's 100 MB) never applies: the API sees only small
// JSON requests. A Journal keeps what was sent, so an upload interrupted by a
// closed tab or a lost connection continues where it stopped.

import { Sha256 } from "./sha256.ts";

export { Sha256 };

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

export type ClientOptions = {
  baseUrl: string;
  token: string;
  organizationId?: string;
  fetch?: typeof fetch;
  // Tries per API request when the answer can change: a network error, 408,
  // 429 or 5xx. Default 5.
  attempts?: number;
  // Waits between tries; tests pass one that does not really wait.
  sleep?: (ms: number) => Promise<void>;
};

// Exponential backoff with full jitter, capped at 30 seconds: anywhere from
// nothing up to the doubled delay, so clients that failed together do not
// all come back at once.
export function backoffMs(attempt: number, random = Math.random) {
  return random() * Math.min(30_000, 1000 * 2 ** attempt);
}

function retryableStatus(status: number) {
  return status === 408 || status === 429 || status >= 500;
}

// Retry-After in seconds or as a date, at most a minute.
function retryAfterMs(headers: Headers) {
  const value = headers.get("Retry-After");
  if (!value) return undefined;
  const seconds = Number(value);
  const ms = Number.isFinite(seconds) ? seconds * 1000 : Date.parse(value) - Date.now();
  return Number.isFinite(ms) && ms > 0 ? Math.min(ms, 60_000) : undefined;
}

const realSleep = (ms: number) => new Promise<void>((done) => setTimeout(done, ms));

// Waits, or stops at once when the signal aborts.
function sleepUnless(ms: number, signal?: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

// The API's answer when an upload session it was asked about has ended or
// never existed: 404, or 409 "not active" (a multipart upload that is not
// active, or a replayed start whose session ended). Other 409s (a start
// still pending, an idempotency conflict) are not this.
function sessionGone(err: unknown) {
  if (!(err instanceof ApiError)) return false;
  return err.status === 404 || err.status === 410 || (err.status === 409 && /not active/i.test(err.message));
}

export class Client {
  readonly baseUrl: string;
  private readonly token: string;
  private readonly organizationId?: string;
  readonly fetch: typeof fetch;
  readonly sleep: (ms: number) => Promise<void>;
  private readonly attempts: number;

  constructor(options: ClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/+$/, "");
    this.token = options.token;
    this.organizationId = options.organizationId;
    this.fetch = options.fetch ?? globalThis.fetch.bind(globalThis);
    this.sleep = options.sleep ?? realSleep;
    this.attempts = options.attempts ?? 5;
  }

  // The upload routes are safe to send again: starts carry an idempotency
  // key, and part links, confirmations and completion can be repeated.
  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.token}` };
    if (this.organizationId) headers["X-Organization-ID"] = this.organizationId;
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let res: Response;
    for (let attempt = 1; ; attempt++) {
      try {
        res = await this.fetch(`${this.baseUrl}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
      } catch (err) {
        if ((err as Error).name === "AbortError" || attempt >= this.attempts) throw err;
        await this.sleep(backoffMs(attempt - 1));
        continue;
      }
      if (res.ok || !retryableStatus(res.status) || attempt >= this.attempts) break;
      await this.sleep(retryAfterMs(res.headers) ?? backoffMs(attempt - 1));
    }
    if (!res.ok) {
      let message = res.statusText;
      let code: string | undefined;
      try {
        const problem = (await res.json()) as { error?: string; code?: string };
        message = problem.error ?? message;
        code = problem.code;
      } catch {
        // not JSON: keep the status text
      }
      throw new ApiError(res.status, message, code);
    }
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }
}

// What an upload needs of a file. A browser File or Blob is one; ./node has
// one for a path on disk.
export type Source = {
  size: number;
  name: string;
  type?: string;
  read(start: number, end: number): Promise<Uint8Array>;
};

export function blobSource(blob: Blob, name?: string): Source {
  return {
    size: blob.size,
    name: name ?? (blob as File).name ?? "file",
    type: blob.type || undefined,
    read: async (start, end) => new Uint8Array(await blob.slice(start, end).arrayBuffer()),
  };
}

// Where an unfinished upload's progress is kept: localStorage in a browser,
// a file in Node. Keys are the file's SHA-256 and destination.
export type Journal = {
  get(key: string): Promise<JournalEntry | undefined> | JournalEntry | undefined;
  set(key: string, entry: JournalEntry): Promise<void> | void;
  remove(key: string): Promise<void> | void;
};

export type JournalEntry = { uploadId: string; partSize: number; parts: Record<string, string> };

export class MemoryJournal implements Journal {
  private entries = new Map<string, JournalEntry>();
  get(key: string) {
    return this.entries.get(key);
  }
  set(key: string, entry: JournalEntry) {
    this.entries.set(key, entry);
  }
  remove(key: string) {
    this.entries.delete(key);
  }
}

export function localStorageJournal(storage: Pick<Storage, "getItem" | "setItem" | "removeItem">, prefix = "osp-upload:"): Journal {
  return {
    get(key) {
      const raw = storage.getItem(prefix + key);
      return raw ? (JSON.parse(raw) as JournalEntry) : undefined;
    },
    set(key, entry) {
      storage.setItem(prefix + key, JSON.stringify(entry));
    },
    remove(key) {
      storage.removeItem(prefix + key);
    },
  };
}

// Parts are 8 MiB unless a file needs more of them than S3 allows. Parts go to
// storage, not through the API, so proxy limits do not bound them; a browser
// holds one part per parallel request in memory.
export const DEFAULT_PART_SIZE = 8 * 1024 * 1024;
const MAX_PARTS = 10000;
const SINGLE_UPLOAD_LIMIT = 64 * 1024 * 1024;
const HASH_BLOCK = 4 * 1024 * 1024;

export function partSizeFor(size: number, wanted = DEFAULT_PART_SIZE) {
  const least = Math.ceil(size / MAX_PARTS);
  return Math.max(wanted, least);
}

export async function sha256OfSource(source: Source, onProgress?: (done: number) => void) {
  const hash = new Sha256();
  for (let at = 0; at < source.size; at += HASH_BLOCK) {
    hash.update(await source.read(at, Math.min(at + HASH_BLOCK, source.size)));
    onProgress?.(Math.min(at + HASH_BLOCK, source.size));
  }
  return hash.hex();
}

export type UploadOptions = {
  dataset?: string;
  path?: string;
  contentType?: string;
  metadata?: Record<string, string>;
  storageConnectionId?: string;
  journal?: Journal;
  partSize?: number;
  concurrency?: number;
  attempts?: number;
  // fetch cannot report upload progress, so a request to storage counts as
  // stalled when it runs this much longer than 16 KiB/s would need for its
  // bytes. It is stopped and tried again. Default 30 seconds.
  stallMs?: number;
  signal?: AbortSignal;
  onProgress?: (p: { phase: "hashing" | "uploading"; bytes: number; total: number }) => void;
  // Wait for the file's checks and answer its asset; otherwise answer as soon
  // as the bytes are in.
  wait?: boolean;
  pollMs?: number;
  // How long to wait for the checks before answering the status so far.
  // Default 10 minutes.
  waitTimeoutMs?: number;
};

export type UploadResult = { uploadId: string; sha256: string; status: string; asset?: { id: string; status: string; rejection_reason?: string } };

type UploadStatus = { status: string; asset?: { id: string; status: string; rejection_reason?: string } };

const STALL_MS = 30_000;
const WAIT_TIMEOUT_MS = 10 * 60_000;
// Asset states that do not change by waiting. BLOCKED is an AVAILABLE asset
// whose reads a later check stopped.
const ASSET_DONE = new Set(["AVAILABLE", "REJECTED", "BLOCKED", "SOFT_DELETED", "DELETED"]);
// How S3, GCS and Azure word an expired signature or a skewed clock. S3 sends
// an expired URL as AccessDenied, so the code alone means a real refusal.
const EXPIRED_URL =
  /Request has expired|ExpiredToken|token has expired|signature (has )?expired|not valid in the specified time frame|RequestTimeTooSkewed/i;
const SLOWEST_BYTES_PER_SECOND = 16 * 1024;

type PutTarget = { url: string; headers?: Record<string, string>; refresh: () => Promise<string> };
type PutOptions = { attempts: number; stallMs: number; signal?: AbortSignal };

// putWithRetry sends bytes to a presigned URL and answers the ETag. Network
// errors, stalls, 408, 429 and 5xx are tried again with backoff (Retry-After
// wins); an expired URL is replaced by refresh(); any other refusal would
// fail the same way again and is thrown at once.
async function putWithRetry(client: Client, target: PutTarget, body: Uint8Array, options: PutOptions) {
  const { attempts, stallMs, signal } = options;
  let url = target.url;
  let last: unknown;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    signal?.throwIfAborted();
    const deadline = AbortSignal.timeout(Math.ceil(stallMs + (body.length / SLOWEST_BYTES_PER_SECOND) * 1000));
    let res: Response | undefined;
    try {
      res = await client.fetch(url, {
        method: "PUT",
        body: body as BodyInit,
        headers: target.headers ?? {},
        signal: signal ? AbortSignal.any([signal, deadline]) : deadline,
      });
    } catch (err) {
      if (signal?.aborted) throw err;
      last = err;
    }
    if (res?.ok) return res.headers.get("ETag") ?? "";
    let wait: number | undefined;
    if (res) {
      const text = await res.text().catch(() => "");
      if (res.status === 403 && EXPIRED_URL.test(text)) {
        url = await target.refresh();
        last = new ApiError(403, "the storage URL expired");
        continue;
      }
      if (!retryableStatus(res.status)) throw new ApiError(res.status, `storage refused the upload (${res.status})`);
      last = new ApiError(res.status, `storage answered ${res.status}`);
      wait = retryAfterMs(res.headers);
    }
    if (attempt < attempts) await client.sleep(wait ?? backoffMs(attempt - 1));
  }
  throw last;
}

// upload sends a file and answers once it is complete. Run it again with the
// same journal after an interruption: parts already sent are not sent again.
export async function upload(client: Client, input: Source | Blob, options: UploadOptions = {}): Promise<UploadResult> {
  const source: Source = input instanceof Blob ? blobSource(input) : input;
  const onProgress = options.onProgress;
  const sha256 = await sha256OfSource(source, (bytes) => onProgress?.({ phase: "hashing", bytes, total: source.size }));
  const path = options.path ?? source.name;
  const body = {
    file_name: source.name,
    content_type: options.contentType ?? source.type ?? "application/octet-stream",
    size_bytes: source.size,
    checksum_sha256: sha256,
    idempotency_key: `js-${sha256.slice(0, 40)}-${await keyOf(`${options.dataset ?? ""}/${path}`)}`,
    dataset: options.dataset,
    path,
    metadata: options.metadata,
    storage_connection_id: options.storageConnectionId,
  };
  const put: PutOptions = { attempts: options.attempts ?? 5, stallMs: options.stallMs ?? STALL_MS, signal: options.signal };
  let uploadId: string;
  if (source.size <= SINGLE_UPLOAD_LIMIT && !options.partSize) {
    try {
      uploadId = await sendSingle(client, source, body, put, onProgress);
    } catch (err) {
      if (!sessionGone(err)) throw err;
      // The session ended, and its key would hand it back: once more from
      // the start, under a new key.
      const fresh = { ...body, idempotency_key: `${body.idempotency_key}-${crypto.randomUUID().slice(0, 8)}` };
      uploadId = await sendSingle(client, source, fresh, put, onProgress);
    }
  } else {
    uploadId = await sendParts(client, source, body, sha256, options, put);
  }
  if (!options.wait) return { uploadId, sha256, status: "COMPLETING" };
  const poll = options.pollMs ?? 1000;
  const giveUpAt = Date.now() + (options.waitTimeoutMs ?? WAIT_TIMEOUT_MS);
  for (;;) {
    options.signal?.throwIfAborted();
    const status = await client.request<UploadStatus>("GET", `/api/v1/uploads/${uploadId}`);
    const state = status.asset?.status;
    if (state && ASSET_DONE.has(state)) return { uploadId, sha256, status: state, asset: status.asset };
    if (status.status === "FAILED" || status.status === "ABORTED" || status.status === "EXPIRED") return { uploadId, sha256, status: status.status };
    // Still being checked: answer what the server says so far, and the
    // caller can ask again later.
    if (Date.now() + poll > giveUpAt) return { uploadId, sha256, status: status.status, asset: status.asset };
    await sleepUnless(poll, options.signal);
  }
}

async function sendSingle(
  client: Client,
  source: Source,
  body: { content_type: string } & Record<string, unknown>,
  put: PutOptions,
  onProgress: UploadOptions["onProgress"],
) {
  type Created = { upload_id: string; status?: string; upload_url: string; upload_headers?: Record<string, string> };
  const created = await client.request<Created>("POST", "/api/v1/uploads", body);
  // A start sent again after its upload completed: nothing is left to send.
  if (!created.upload_url && (created.status === "COMPLETED" || created.status === "VERIFIED")) return created.upload_id;
  const data = await source.read(0, source.size);
  const headers = { "Content-Type": body.content_type, ...(created.upload_headers ?? {}) };
  // The same idempotency key hands back the same upload with a new URL.
  const refresh = async () => (await client.request<Created>("POST", "/api/v1/uploads", body)).upload_url;
  await putWithRetry(client, { url: created.upload_url, headers, refresh }, data, put);
  onProgress?.({ phase: "uploading", bytes: source.size, total: source.size });
  await client.request("POST", `/api/v1/uploads/${created.upload_id}/complete`);
  return created.upload_id;
}

async function keyOf(text: string) {
  const h = new Sha256();
  h.update(new TextEncoder().encode(text));
  return h.hex().slice(0, 24);
}

async function sendParts(client: Client, source: Source, body: Record<string, unknown>, sha256: string, options: UploadOptions, put: PutOptions) {
  const journal = options.journal ?? new MemoryJournal();
  const key = `${sha256}:${body.dataset ?? ""}:${body.path}`;
  const size = partSizeFor(source.size, options.partSize);
  const saved = await journal.get(key);
  const resumed = saved && saved.partSize === size ? saved : undefined;
  try {
    return await sendSession(client, source, body, sha256, options, put, journal, key, resumed);
  } catch (err) {
    if (!sessionGone(err)) throw err;
    // The session ended (expired, aborted or unknown), so a re-run must not
    // try it again.
    await journal.remove(key);
    // Once more from the beginning, under a new start key: the old one would
    // hand back the ended session, even with no journal entry.
    const fresh = { ...body, idempotency_key: `${body.idempotency_key}-${crypto.randomUUID().slice(0, 8)}` };
    return sendSession(client, source, fresh, sha256, options, put, journal, key, undefined);
  }
}

async function sendSession(
  client: Client,
  source: Source,
  body: Record<string, unknown>,
  sha256: string,
  options: UploadOptions,
  put: PutOptions,
  journal: Journal,
  key: string,
  resumed: JournalEntry | undefined,
) {
  const size = partSizeFor(source.size, options.partSize);
  let entry = resumed;
  if (!entry) {
    const started = await client.request<{ upload_id: string }>("POST", "/api/v1/uploads/multipart", body);
    entry = { uploadId: started.upload_id, partSize: size, parts: {} };
    await journal.set(key, entry);
  }
  const current = entry;
  const count = Math.ceil(source.size / size);
  const missing: number[] = [];
  let sent = 0;
  for (let n = 1; n <= count; n++) {
    if (current.parts[n]) sent += Math.min(size, source.size - (n - 1) * size);
    else missing.push(n);
  }
  let saving = Promise.resolve();
  // The first failure stops the other workers too, and upload() answers only
  // once they have all stopped, so nothing writes the journal after it.
  const stop = new AbortController();
  const partPut = { ...put, signal: put.signal ? AbortSignal.any([put.signal, stop.signal]) : stop.signal };
  let failure: { err: unknown } | undefined;
  const worker = async () => {
    try {
      while (!stop.signal.aborted) {
        const batch = missing.splice(0, 8);
        if (batch.length === 0) return;
        const partUrls = (numbers: number[]) =>
          client.request<{ parts: { part_number: number; url: string }[] }>("POST", `/api/v1/uploads/${current.uploadId}/parts/urls`, { part_numbers: numbers });
        const { parts } = await partUrls(batch);
        for (const part of parts) {
          if (stop.signal.aborted) return;
          const start = (part.part_number - 1) * size;
          const end = Math.min(start + size, source.size);
          // Only this part gets a new URL when its own has expired.
          const refresh = async () => (await partUrls([part.part_number])).parts[0].url;
          const etag = await putWithRetry(client, { url: part.url, refresh }, await source.read(start, end), partPut);
          await client.request("POST", `/api/v1/uploads/${current.uploadId}/parts/confirm`, {
            parts: [{ part_number: part.part_number, etag, size_bytes: end - start }],
          });
          current.parts[part.part_number] = etag;
          // Saved in order, so two parts finishing together do not race.
          saving = saving.then(() => journal.set(key, current));
          await saving;
          sent += end - start;
          options.onProgress?.({ phase: "uploading", bytes: sent, total: source.size });
        }
      }
    } catch (err) {
      failure ??= { err };
      stop.abort();
    }
  };
  await Promise.all(Array.from({ length: options.concurrency ?? 4 }, worker));
  if (failure) throw failure.err;
  const completed = Object.entries(current.parts)
    .map(([n, etag]) => ({ part_number: Number(n), etag }))
    .sort((a, b) => a.part_number - b.part_number);
  await client.request("POST", `/api/v1/uploads/${current.uploadId}/complete`, { parts: completed, checksum_sha256: sha256 });
  await journal.remove(key);
  return current.uploadId;
}
