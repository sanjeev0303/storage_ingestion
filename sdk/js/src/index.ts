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
};

export class Client {
  readonly baseUrl: string;
  private readonly token: string;
  private readonly organizationId?: string;
  readonly fetch: typeof fetch;

  constructor(options: ClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/+$/, "");
    this.token = options.token;
    this.organizationId = options.organizationId;
    this.fetch = options.fetch ?? globalThis.fetch.bind(globalThis);
  }

  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = { Authorization: `Bearer ${this.token}` };
    if (this.organizationId) headers["X-Organization-ID"] = this.organizationId;
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await this.fetch(`${this.baseUrl}${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
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
  signal?: AbortSignal;
  onProgress?: (p: { phase: "hashing" | "uploading"; bytes: number; total: number }) => void;
  // Wait for the file's checks and answer its asset; otherwise answer as soon
  // as the bytes are in.
  wait?: boolean;
  pollMs?: number;
};

export type UploadResult = { uploadId: string; sha256: string; status: string; asset?: { id: string; status: string; rejection_reason?: string } };

type UploadStatus = { status: string; asset?: { id: string; status: string; rejection_reason?: string } };

async function putWithRetry(client: Client, url: string, body: Uint8Array, headers: Record<string, string>, attempts: number, signal?: AbortSignal) {
  let last: unknown;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    signal?.throwIfAborted();
    try {
      const res = await client.fetch(url, { method: "PUT", body: body as BodyInit, headers, signal });
      if (res.ok) return res.headers.get("ETag") ?? "";
      if (res.status < 500 && res.status !== 429) throw new ApiError(res.status, `storage refused the upload (${res.status})`);
      last = new ApiError(res.status, `storage answered ${res.status}`);
    } catch (err) {
      if (err instanceof ApiError && err.status < 500 && err.status !== 429) throw err;
      if ((err as Error).name === "AbortError") throw err;
      last = err;
    }
    await new Promise((done) => setTimeout(done, Math.min(250 * 2 ** attempt, 8000)));
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
  const attempts = options.attempts ?? 5;
  let uploadId: string;
  if (source.size <= SINGLE_UPLOAD_LIMIT && !options.partSize) {
    const created = await client.request<{ upload_id: string; upload_url: string; upload_headers?: Record<string, string> }>("POST", "/api/v1/uploads", body);
    const data = await source.read(0, source.size);
    const headers = { "Content-Type": body.content_type, ...(created.upload_headers ?? {}) };
    await putWithRetry(client, created.upload_url, data, headers, attempts, options.signal);
    onProgress?.({ phase: "uploading", bytes: source.size, total: source.size });
    await client.request("POST", `/api/v1/uploads/${created.upload_id}/complete`);
    uploadId = created.upload_id;
  } else {
    uploadId = await sendParts(client, source, body, sha256, options, attempts);
  }
  if (!options.wait) return { uploadId, sha256, status: "COMPLETING" };
  const poll = options.pollMs ?? 1000;
  for (;;) {
    options.signal?.throwIfAborted();
    const status = await client.request<UploadStatus>("GET", `/api/v1/uploads/${uploadId}`);
    const state = status.asset?.status;
    if (state === "AVAILABLE" || state === "REJECTED") return { uploadId, sha256, status: state, asset: status.asset };
    if (status.status === "FAILED" || status.status === "ABORTED" || status.status === "EXPIRED") return { uploadId, sha256, status: status.status };
    await new Promise((done) => setTimeout(done, poll));
  }
}

async function keyOf(text: string) {
  const h = new Sha256();
  h.update(new TextEncoder().encode(text));
  return h.hex().slice(0, 24);
}

async function sendParts(client: Client, source: Source, body: Record<string, unknown>, sha256: string, options: UploadOptions, attempts: number) {
  const journal = options.journal ?? new MemoryJournal();
  const key = `${sha256}:${body.dataset ?? ""}:${body.path}`;
  const size = partSizeFor(source.size, options.partSize);
  let entry = await journal.get(key);
  if (!entry || entry.partSize !== size) {
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
  const worker = async () => {
    for (;;) {
      const batch = missing.splice(0, 8);
      if (batch.length === 0) return;
      const { parts } = await client.request<{ parts: { part_number: number; url: string }[] }>("POST", `/api/v1/uploads/${current.uploadId}/parts/urls`, { part_numbers: batch });
      for (const part of parts) {
        const start = (part.part_number - 1) * size;
        const end = Math.min(start + size, source.size);
        const etag = await putWithRetry(client, part.url, await source.read(start, end), {}, attempts, options.signal);
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
  };
  await Promise.all(Array.from({ length: options.concurrency ?? 4 }, worker));
  const completed = Object.entries(current.parts)
    .map(([n, etag]) => ({ part_number: Number(n), etag }))
    .sort((a, b) => a.part_number - b.part_number);
  await client.request("POST", `/api/v1/uploads/${current.uploadId}/complete`, { parts: completed, checksum_sha256: sha256 });
  await journal.remove(key);
  return current.uploadId;
}
