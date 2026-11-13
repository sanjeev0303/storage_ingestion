import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setActiveOrganization } from "@/lib/api/client";
import { MIB } from "@/lib/upload/parts";
import { UploadHttpError, UploadNetworkError } from "@/lib/upload/put";
import { abortError } from "@/lib/upload/sha256";
import { API, server } from "@/test/server";
import type { ResumeRecord } from "../types";
import { FileUpload, HEAD_BYTES, isSameFile } from "./file-upload";
import { backoffMs, ConnectionWatch, preferredPartBytes, SLOW_BPS } from "./network";
import { memoryStore } from "./store";

const SHA = "a".repeat(64);
const request = { file_name: "big.bin", content_type: "application/octet-stream" };

type Log = { method: string; path: string; body?: unknown; org?: string | null }[];

type Mock = {
  log: Log;
  catalog: string[];
  uploadStatuses: string[];
  failPartUrls?: () => Response | null;
  startStatus?: () => Response | null;
};

// A small fake of the API: records every call and answers the happy path.
function mockApi(overrides: Partial<Mock> = {}) {
  const mock: Mock = { log: [], catalog: ["QUARANTINED", "PROCESSING", "AVAILABLE"], uploadStatuses: ["COMPLETED", "VERIFIED"], ...overrides };
  let completed = false;

  const record = async (request: Request) => {
    const url = new URL(request.url);
    const text = await request.clone().text();
    mock.log.push({
      method: request.method,
      path: url.pathname.replace("/api/proxy", ""),
      body: text ? JSON.parse(text) : undefined,
      org: request.headers.get("X-Organization-ID"),
    });
  };

  server.use(
    http.post(`${API}/uploads/multipart`, async ({ request }) => {
      await record(request);
      const early = mock.startStatus?.();
      if (early) return early;
      return HttpResponse.json({ upload_id: "mp1", status: "PENDING", expires_at: "2099-01-01T00:00:00Z", reused: false, storage_connection_id: "platform" }, { status: 201 });
    }),
    http.post(`${API}/uploads/:id/parts/urls`, async ({ request }) => {
      await record(request);
      const early = mock.failPartUrls?.();
      if (early) return early;
      const { part_numbers } = (await request.clone().json()) as { part_numbers: number[] };
      return HttpResponse.json({
        upload_id: "mp1",
        parts: part_numbers.map((n, i) => ({ part_number: n, url: `https://store.test/part/${n}/v${mock.log.filter((l) => l.path.endsWith("parts/urls")).length}-${i}`, expires_at: "2099-01-01T00:00:00Z" })),
      });
    }),
    http.post(`${API}/uploads/:id/parts/confirm`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ status: "parts_recorded" });
    }),
    http.post(`${API}/uploads/:id/complete`, async ({ request }) => {
      await record(request);
      completed = true;
      return HttpResponse.json({ upload_id: "mp1", status: "COMPLETED", expires_at: "2099-01-01T00:00:00Z" });
    }),
    http.post(`${API}/uploads/:id/abort`, async ({ request }) => {
      await record(request);
      return HttpResponse.json({ upload_id: "mp1", status: "ABORTED", expires_at: "2099-01-01T00:00:00Z" });
    }),
    http.post(`${API}/uploads`, async ({ request }) => {
      await record(request);
      return HttpResponse.json(
        { upload_id: "sg1", status: "PENDING", upload_url: "https://store.test/single", expires_at: "2099-01-01T00:00:00Z", upload_headers: { "x-ms-blob-type": "BlockBlob" }, storage_connection_id: "platform" },
        { status: 201 },
      );
    }),
    http.get(`${API}/uploads/:id`, async ({ request }) => {
      await record(request);
      const status = mock.uploadStatuses.length > 1 ? mock.uploadStatuses.shift() : mock.uploadStatuses[0];
      // Once verified, the upload reports its asset, step by step.
      let asset;
      if (status === "VERIFIED") {
        const assetStatus = mock.catalog.length > 1 ? mock.catalog.shift() : mock.catalog[0];
        asset = { asset_id: "as1", status: assetStatus, object_id: "obj1", version: 1, rejection_reason: assetStatus === "REJECTED" ? "contains a virus" : undefined };
      }
      return HttpResponse.json({ upload_id: "mp1", status, file_name: "big.bin", size_bytes: 1, expires_at: "2099-01-01T00:00:00Z", multipart: true, asset });
    }),
    http.get(`${API}/catalog/lookup`, async ({ request }) => {
      await record(request);
      if (!completed) return HttpResponse.json({ error: "not found", code: "not_found" }, { status: 404 });
      return HttpResponse.json({ id: "obj1", dataset: "", path: "big.bin", versions: 1 });
    }),
    http.get(`${API}/catalog/objects/:id/versions`, async ({ request }) => {
      await record(request);
      const status = mock.catalog.length > 1 ? mock.catalog.shift() : mock.catalog[0];
      return HttpResponse.json({ versions: [{ version: 1, asset_id: "as1", status, checksum_sha256: SHA, size_bytes: 1 }] });
    }),
    http.get(`${API}/assets/:id`, () => HttpResponse.json({ id: "as1", rejection_reason: "contains a virus" })),
  );
  return mock;
}

function bigFile(size = 20 * MIB, name = "big.bin") {
  return new File([new Uint8Array(size)], name, { type: "application/octet-stream" });
}

// A connection watch whose API check answers as the test says. It never
// checks on a timer: tests call recheck() or fire the browser's events.
function fakeConnection(reachable = { value: true }) {
  return new ConnectionWatch({ probe: async () => reachable.value, sleep: () => new Promise(() => undefined) });
}

function makeUpload(file: File, put: unknown, extra = {}, store = memoryStore()) {
  const sleep = vi.fn(async () => undefined);
  const upload = new FileUpload(file, request, {
    put: put as never,
    hash: async () => SHA,
    sleep,
    store,
    connection: fakeConnection(),
    pollMs: 0,
    ...extra,
  });
  return { upload, sleep, store };
}

// A PUT that waits until its request is aborted, like one on a dead link.
function hangingPut(options?: { signal?: AbortSignal }) {
  return new Promise<{ status: number; etag: string | null }>((_resolve, reject) => {
    options?.signal?.addEventListener("abort", () => reject(abortError()));
  });
}

const okPut = async (_url: string, body: Blob) => ({ status: 200, etag: `"etag-${body.size}"` });

beforeEach(() => setActiveOrganization("org-1"));

describe("multipart upload", () => {
  it("hashes the file while its parts upload and sends the checksum with the completion", async () => {
    const mock = mockApi();
    let finishHash!: (sha: string) => void;
    // The first MiB is hashed at once, to know the file again on resume.
    const hash = vi.fn((blob: Blob) => (blob.size <= HEAD_BYTES ? Promise.resolve("head") : new Promise<string>((resolve) => (finishHash = resolve))));
    const store = memoryStore();
    const saved = vi.spyOn(store, "put");
    const putsBeforeHash: string[] = [];
    let hashed = false;
    const put = vi.fn(async (url: string, body: Blob) => {
      if (!hashed) putsBeforeHash.push(url);
      return { status: 200, etag: `"etag-${body.size}"` };
    });
    const { upload } = makeUpload(bigFile(), put as never, { hash }, store);

    const running = upload.run();
    // Every part goes up while the checksum is still being read.
    await vi.waitFor(() => expect(putsBeforeHash).toHaveLength(3));
    expect(mock.log.some((l) => l.path.endsWith("/complete"))).toBe(false);
    expect(upload.getSnapshot().state).toBe("finalizing");

    hashed = true;
    finishHash(SHA);
    await running;
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(hash.mock.calls.filter(([blob]) => blob.size > HEAD_BYTES)).toHaveLength(1);
    expect(mock.log.find((l) => l.path === "/uploads/mp1/complete")?.body).toMatchObject({ checksum_sha256: SHA });
    // Saved for a resume after a reload, so it is not read again.
    expect(saved.mock.calls.some(([record]) => record.sha256 === SHA)).toBe(true);
  });

  it("retries a failed part, confirms in batches, completes in order and waits for the catalog", async () => {
    const mock = mockApi();
    let failedOnce = false;
    const put = vi.fn(async (url: string, body: Blob) => {
      if (url.includes("/part/2/") && !failedOnce) {
        failedOnce = true;
        throw new UploadNetworkError();
      }
      return okPut(url, body);
    });
    const { upload } = makeUpload(bigFile(), put as never);

    await upload.run();
    await upload.settled();

    const snap = upload.getSnapshot();
    expect(snap.state).toBe("available");
    expect(snap.asset).toMatchObject({ objectId: "obj1", status: "AVAILABLE" });
    expect(put.mock.calls.filter(([url]) => String(url).includes("/part/2/"))).toHaveLength(2);

    // The file is hashed while its parts upload, so the checksum goes with
    // the completion rather than the start.
    const start = mock.log.find((l) => l.path === "/uploads/multipart");
    expect(start?.body).toMatchObject({ size_bytes: 20 * MIB, idempotency_key: expect.any(String) });
    expect(start?.body).not.toHaveProperty("checksum_sha256");

    const complete = mock.log.find((l) => l.path === "/uploads/mp1/complete");
    expect(complete?.body).toEqual({
      parts: [
        { part_number: 1, etag: `"etag-${8 * MIB}"` },
        { part_number: 2, etag: `"etag-${8 * MIB}"` },
        { part_number: 3, etag: `"etag-${4 * MIB}"` },
      ],
      checksum_sha256: SHA,
    });
    expect(mock.log.filter((l) => l.path.endsWith("/parts/confirm"))).toHaveLength(1);
  });

  it("asks for a fresh URL for the part whose URL expired and nothing else", async () => {
    const mock = mockApi();
    let expiredOnce = false;
    const put = vi.fn(async (url: string, body: Blob) => {
      if (url.includes("/part/2/") && !expiredOnce) {
        expiredOnce = true;
        throw new UploadHttpError(403, "Request has expired");
      }
      return okPut(url, body);
    });
    const { upload } = makeUpload(bigFile(), put as never);

    await upload.run();
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    const presigns = mock.log.filter((l) => l.path.endsWith("/parts/urls")).map((l) => l.body);
    expect(presigns).toEqual([{ part_numbers: [1, 2, 3] }, { part_numbers: [2] }]);
    expect(mock.log.filter((l) => l.path.endsWith("/parts/confirm"))).toHaveLength(1);
  });

  it("cancelling aborts the in-flight PUT and calls /abort", async () => {
    const mock = mockApi();
    let started: () => void = () => undefined;
    const putStarted = new Promise<void>((resolve) => (started = resolve));
    const put = vi.fn(
      (_url: string, _body: Blob, options?: { signal?: AbortSignal }) =>
        new Promise<{ status: number; etag: string | null }>((_resolve, reject) => {
          started();
          options?.signal?.addEventListener("abort", () => reject(abortError()));
        }),
    );
    const { upload, store } = makeUpload(bigFile(), put as never);

    const running = upload.run();
    await putStarted;
    upload.cancel();
    await running;

    expect(upload.getSnapshot().state).toBe("cancelled");
    await vi.waitFor(() => expect(mock.log.some((l) => l.path === "/uploads/mp1/abort")).toBe(true));
    expect(await store.get("mp1")).toBeUndefined();
  });

  it("resumes from the stored parts and only sends the missing ones", async () => {
    const mock = mockApi({ uploadStatuses: ["UPLOADING", "COMPLETED", "VERIFIED"] });
    const file = bigFile();
    const saved: ResumeRecord = {
      upload_id: "mp1",
      organization_id: "org-1",
      idempotency_key: "key-from-before",
      request,
      file_size: file.size,
      file_last_modified: file.lastModified,
      sha256: SHA,
      part_size: 8 * MIB,
      parts: [{ number: 1, etag: '"stored-1"', size: 8 * MIB }],
      expires_at: "2099-01-01T00:00:00Z",
      saved_at: "2026-10-22T00:00:00Z",
    };
    const put = vi.fn(okPut);
    const hash = vi.fn(async () => SHA);
    const { upload } = makeUpload(file, put as never, { hash });

    await upload.run(saved);
    await upload.settled();

    expect(hash).not.toHaveBeenCalled();
    expect(put).toHaveBeenCalledTimes(2);
    expect(mock.log.some((l) => l.path === "/uploads/multipart")).toBe(false);
    expect(mock.log.find((l) => l.path.endsWith("/parts/urls"))?.body).toEqual({ part_numbers: [2, 3] });
    const complete = mock.log.find((l) => l.path.endsWith("/complete"));
    expect((complete?.body as { parts: { part_number: number; etag: string }[] }).parts[0]).toEqual({ part_number: 1, etag: '"stored-1"' });
    expect(upload.getSnapshot().state).toBe("available");
  });

  it("starts again from scratch when the stored session is gone", async () => {
    const mock = mockApi();
    server.use(http.get(`${API}/uploads/old`, () => HttpResponse.json({ error: "upload not found" }, { status: 404 })));
    const file = bigFile();
    const saved: ResumeRecord = {
      upload_id: "old",
      organization_id: "org-1",
      idempotency_key: "old-key",
      request,
      file_size: file.size,
      file_last_modified: file.lastModified,
      sha256: SHA,
      part_size: 8 * MIB,
      parts: [{ number: 1, etag: '"x"', size: 8 * MIB }],
      expires_at: "2099-01-01T00:00:00Z",
      saved_at: "2026-10-22T00:00:00Z",
    };
    const { upload } = makeUpload(file, okPut as never);

    await upload.run(saved);
    await upload.settled();

    const start = mock.log.find((l) => l.path === "/uploads/multipart");
    expect((start?.body as { idempotency_key: string }).idempotency_key).not.toBe("old-key");
    expect(upload.getSnapshot().resumed).toBe(false);
    expect(upload.getSnapshot().state).toBe("available");
  });

  it("reports an expired session and restarts with a new idempotency key", async () => {
    let expired = true;
    const mock = mockApi({
      failPartUrls: () => (expired ? HttpResponse.json({ error: "multipart upload is not active" }, { status: 409 }) : null),
    });
    const { upload } = makeUpload(bigFile(), okPut as never);

    await upload.run();
    await upload.settled();
    expect(upload.getSnapshot().state).toBe("failed");
    expect(upload.getSnapshot().failure?.kind).toBe("session_expired");

    expired = false;
    await upload.restart();
    await upload.settled();

    const keys = mock.log.filter((l) => l.path === "/uploads/multipart").map((l) => (l.body as { idempotency_key: string }).idempotency_key);
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
    expect(upload.getSnapshot().state).toBe("available");
  });

  it("fails with an explanation when the provider does not expose ETag, without retrying", async () => {
    mockApi();
    const put = vi.fn(async () => ({ status: 200, etag: null }));
    const { upload } = makeUpload(bigFile(), put as never);

    await upload.run();
    await upload.settled();

    expect(upload.getSnapshot().failure?.kind).toBe("cors");
    expect(upload.getSnapshot().failure?.message).toMatch(/ExposeHeaders/);
    expect(put.mock.calls.length).toBeLessThanOrEqual(4);
  });

  it("waits for Retry-After on 429 and then continues", async () => {
    let calls = 0;
    mockApi({
      startStatus: () => (calls++ === 0 ? HttpResponse.json({ error: "too many requests", code: "rate_limited" }, { status: 429, headers: { "Retry-After": "7" } }) : null),
    });
    const { upload, sleep } = makeUpload(bigFile(), okPut as never);

    await upload.run();
    await upload.settled();

    expect(sleep).toHaveBeenCalledWith(7000, expect.anything());
    expect(upload.getSnapshot().state).toBe("available");
  });
});

describe("single upload", () => {
  it("sends upload_headers and exactly the declared Content-Type, then completes with no body", async () => {
    const mock = mockApi();
    const put = vi.fn(async () => ({ status: 201, etag: null }));
    const file = new File(["a,b\n1,2\n"], "data.csv", { type: "text/csv" });
    const sleep = vi.fn(async () => undefined);
    const upload = new FileUpload(file, { file_name: "data.csv", content_type: "text/csv" }, { put: put as never, hash: async () => SHA, sleep, store: memoryStore(), connection: fakeConnection(), pollMs: 0 });

    server.use(http.get(`${API}/catalog/objects/:id/versions`, () => HttpResponse.json({ versions: [{ version: 1, asset_id: "as1", status: "AVAILABLE", checksum_sha256: SHA, size_bytes: 8 }] })));
    server.use(http.get(`${API}/catalog/lookup`, () => HttpResponse.json({ id: "obj1", dataset: "", path: "data.csv", versions: 1 })));
    mock.uploadStatuses = ["VERIFIED"];
    server.use(http.post(`${API}/uploads/sg1/complete`, async ({ request }) => {
      mock.log.push({ method: "POST", path: "/uploads/sg1/complete", body: await request.text() });
      return HttpResponse.json({ upload_id: "sg1", status: "COMPLETED" });
    }));

    await upload.run();
    await upload.settled();

    expect(put).toHaveBeenCalledWith("https://store.test/single", file, expect.objectContaining({ headers: { "x-ms-blob-type": "BlockBlob", "Content-Type": "text/csv" } }));
    expect(mock.log.find((l) => l.path === "/uploads/sg1/complete")?.body).toBe("");
    expect(mock.log.find((l) => l.path === "/uploads")?.body).toMatchObject({ checksum_sha256: SHA, content_type: "text/csv" });
    expect(upload.getSnapshot().mode).toBe("single");
  });

  it("starts a new session under a new key when a retry finds the old one ended", async () => {
    const mock = mockApi({ uploadStatuses: ["VERIFIED"] });
    let creates = 0;
    server.use(
      http.post(`${API}/uploads`, async ({ request }) => {
        const body = (await request.json()) as { idempotency_key: string };
        mock.log.push({ method: "POST", path: "/uploads", body });
        creates += 1;
        // The retry repeats the first key, whose session has ended by now.
        if (creates === 2) {
          return HttpResponse.json({ error: "upload session is not active: it has ended; start again with a new idempotency key", code: "session_ended" }, { status: 409 });
        }
        return HttpResponse.json({ upload_id: `sg${creates}`, status: "PENDING", upload_url: "https://store.test/single", expires_at: "2099-01-01T00:00:00Z", storage_connection_id: "platform" }, { status: 201 });
      }),
    );
    let refuse = true;
    const put = vi.fn(async (url: string, body: Blob) => {
      if (refuse) throw new UploadHttpError(400, "InvalidArgument");
      return okPut(url, body);
    });
    const { upload } = makeUpload(new File(["x"], "x.bin"), put as never);
    await upload.run();
    expect(upload.getSnapshot().state).toBe("failed");

    refuse = false;
    await upload.retry();
    await upload.settled();

    const keys = mock.log.filter((l) => l.path === "/uploads").map((l) => (l.body as { idempotency_key: string }).idempotency_key);
    expect(keys).toHaveLength(3);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[0]);
    expect(upload.getSnapshot().uploadId).toBe("sg3");
    expect(upload.getSnapshot().state).toBe("available");
  });

  it("does not upload again when a retry finds the file already stored", async () => {
    const mock = mockApi({ uploadStatuses: ["VERIFIED"] });
    let creates = 0;
    let completes = 0;
    server.use(
      http.post(`${API}/uploads`, async () => {
        creates += 1;
        // A replay of a completed session: nothing is left to upload.
        if (creates === 2) return HttpResponse.json({ upload_id: "sg1", status: "COMPLETED", upload_url: "", expires_at: "2099-01-01T00:00:00Z", storage_connection_id: "platform" });
        return HttpResponse.json({ upload_id: "sg1", status: "PENDING", upload_url: "https://store.test/single", expires_at: "2099-01-01T00:00:00Z", storage_connection_id: "platform" }, { status: 201 });
      }),
      http.post(`${API}/uploads/sg1/complete`, () => {
        completes += 1;
        // The completion went through, but its answer did not arrive.
        return HttpResponse.json({ error: "bad request" }, { status: 400 });
      }),
    );
    const put = vi.fn(okPut);
    const { upload } = makeUpload(new File(["x"], "x.bin"), put as never);
    await upload.run();
    expect(upload.getSnapshot().state).toBe("failed");

    await upload.retry();
    await upload.settled();

    expect(put).toHaveBeenCalledTimes(1);
    expect(put).not.toHaveBeenCalledWith("", expect.anything(), expect.anything());
    expect(completes).toBe(1);
    expect(mock.log.some((l) => l.path === "/uploads/sg1")).toBe(true);
    expect(upload.getSnapshot().state).toBe("available");
  });

  it("shows a rejected asset with the reason from the catalog", async () => {
    mockApi({ catalog: ["REJECTED"], uploadStatuses: ["VERIFIED"] });
    const file = new File(["x"], "x.bin");
    const upload = new FileUpload(file, { file_name: "big.bin", content_type: "application/octet-stream" }, { put: (async () => ({ status: 200, etag: null })) as never, hash: async () => SHA, sleep: async () => undefined, store: memoryStore(), pollMs: 0 });

    await upload.run();
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("rejected");
    expect(upload.getSnapshot().asset?.rejectionReason).toBe("contains a virus");
  });

  it("frees the queue once stored and learns a rejection the catalog never lists from the upload", async () => {
    const mock = mockApi({ uploadStatuses: ["VERIFIED"] });
    // Rejected assets get no catalog version: the catalog has nothing to say.
    server.use(http.get(`${API}/catalog/objects/:id/versions`, () => HttpResponse.json({ versions: [] })));
    const reply: { asset?: object } = {};
    server.use(
      http.get(`${API}/uploads/:id`, () =>
        HttpResponse.json({ upload_id: "sg1", status: "VERIFIED", file_name: "notes.txt", size_bytes: 1, expires_at: "2099-01-01T00:00:00Z", multipart: false, asset: reply.asset }),
      ),
    );
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const sleep = vi.fn(() => gate);
    const upload = new FileUpload(new File(["x"], "notes.txt"), { file_name: "notes.txt", content_type: "text/plain" }, { put: (async () => ({ status: 200, etag: null })) as never, hash: async () => SHA, sleep, store: memoryStore(), pollMs: 3000 });

    await upload.run();
    // run() is back while the server is still deciding.
    expect(["verifying", "processing"]).toContain(upload.getSnapshot().state);
    reply.asset = { asset_id: "as1", status: "REJECTED", rejection_reason: "malware_detected" };
    release();
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("rejected");
    expect(upload.getSnapshot().asset?.rejectionReason).toBe("malware_detected");
    // The first check comes quickly, not after the full poll interval.
    expect(sleep).toHaveBeenCalledWith(500, expect.anything());
    expect(mock.log.some((l) => l.path === "/uploads/sg1/complete")).toBe(true);
  });
});

describe("bad networks", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const putsFor = (put: { mock: { calls: unknown[][] } }, part: number) => put.mock.calls.filter(([url]) => String(url).includes(`/part/${part}/`));
  const confirmed = (mock: Mock) =>
    mock.log.filter((l) => l.path.endsWith("/parts/confirm")).map((l) => (l.body as { parts: { part_number: number }[] }).parts.map((p) => p.part_number));
  const presigned = (mock: Mock) => mock.log.filter((l) => l.path.endsWith("/parts/urls")).map((l) => l.body);

  it("pauses when the browser goes offline, keeps finished parts, and goes on once the API answers again", async () => {
    const mock = mockApi();
    const reachable = { value: true };
    const connection = fakeConnection(reachable);
    let dropped = false;
    // Part 1 gets through; parts 2 and 3 send a little, then hang until the link drops.
    const put = vi.fn((url: string, body: Blob, options?: { signal?: AbortSignal; onProgress?: (n: number) => void }) => {
      if (url.includes("/part/1/") || dropped) return okPut(url, body);
      options?.onProgress?.(MIB);
      return hangingPut(options);
    });
    const { upload } = makeUpload(bigFile(), put as never, { connection });

    const running = upload.run();
    await vi.waitFor(() => expect(upload.getSnapshot().sentBytes).toBe(10 * MIB));
    expect(put).toHaveBeenCalledTimes(3);

    dropped = true;
    reachable.value = false;
    window.dispatchEvent(new Event("offline"));
    expect(upload.getSnapshot().paused).toBe("offline");
    // Only the finished part is kept; the half-sent ones start again.
    expect(upload.getSnapshot().sentBytes).toBe(8 * MIB);

    // The online event alone is not believed while the API does not answer.
    window.dispatchEvent(new Event("online"));
    await connection.check();
    expect(upload.getSnapshot().paused).toBe("offline");

    reachable.value = true;
    window.dispatchEvent(new Event("online"));
    await running;
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(putsFor(put, 1)).toHaveLength(1);
    // The part sent before the drop is confirmed first; only 2 and 3 go again.
    expect(presigned(mock)).toEqual([{ part_numbers: [1, 2, 3] }, { part_numbers: [2, 3] }]);
    expect(confirmed(mock)).toEqual([[1], [2, 3]]);
  });

  it("treats a failed request as the real signal: checks the API and pauses while it cannot be reached", async () => {
    const mock = mockApi();
    const reachable = { value: true };
    const connection = fakeConnection(reachable);
    let recovered = false;
    let failPart2: () => void = () => undefined;
    const put = vi.fn((url: string, body: Blob, options?: { signal?: AbortSignal }) => {
      if (url.includes("/part/1/") || recovered) return okPut(url, body);
      if (url.includes("/part/2/")) {
        return new Promise<{ status: number; etag: string | null }>((_resolve, reject) => (failPart2 = () => reject(new UploadNetworkError())));
      }
      return hangingPut(options);
    });
    const { upload, sleep } = makeUpload(bigFile(), put as never, { connection });

    const running = upload.run();
    await vi.waitFor(() => expect(upload.getSnapshot().sentBytes).toBe(8 * MIB));
    // navigator.onLine still says true; the request says otherwise.
    reachable.value = false;
    failPart2();
    await vi.waitFor(() => expect(upload.getSnapshot().paused).toBe("offline"));
    // No retries are spent while offline.
    expect(sleep).not.toHaveBeenCalled();

    recovered = true;
    reachable.value = true;
    await connection.recheck();
    await running;
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(putsFor(put, 1)).toHaveLength(1);
    expect(presigned(mock)).toEqual([{ part_numbers: [1, 2, 3] }, { part_numbers: [2, 3] }]);
  });

  it("stops a part that sends nothing for 20 seconds and sends it again", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    mockApi();
    let stuck = true;
    const put = vi.fn(async (url: string, body: Blob, options?: { signal?: AbortSignal; onProgress?: (n: number) => void }) => {
      if (url.includes("/part/2/") && stuck) {
        stuck = false;
        options?.onProgress?.(1024);
        return hangingPut(options);
      }
      return okPut(url, body);
    });
    const { upload } = makeUpload(bigFile(), put as never);

    const running = upload.run();
    await vi.waitFor(() => expect(put).toHaveBeenCalledTimes(3));
    // No whole-request timeout: the watchdog decides.
    expect(put.mock.calls[0][2]).toMatchObject({ timeoutMs: 0 });
    await vi.advanceTimersByTimeAsync(19_000);
    expect(putsFor(put, 2)).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1_500);
    await running;
    await upload.settled();

    expect(putsFor(put, 2)).toHaveLength(2);
    expect(upload.getSnapshot().state).toBe("available");
  });

  it("never retries a refusal that would not change", async () => {
    mockApi();
    const put = vi.fn<(url: string) => Promise<never>>(async () => {
      throw new UploadHttpError(400, "InvalidArgument");
    });
    const { upload, sleep } = makeUpload(bigFile(), put as never);

    await upload.run();

    expect(upload.getSnapshot().state).toBe("failed");
    // At most one attempt for each part that had started.
    expect(new Set(put.mock.calls.map(([url]) => url)).size).toBe(put.mock.calls.length);
    expect(sleep).not.toHaveBeenCalled();
  });

  it("backs off with full jitter, doubling up to a 30 second cap", () => {
    expect(backoffMs(0, () => 0)).toBe(0);
    expect(backoffMs(3, () => 0.5)).toBe(4000);
    expect(backoffMs(10, () => 0.999)).toBeGreaterThan(29_000);
    expect(backoffMs(10, () => 0.999)).toBeLessThan(30_000);
    for (let attempt = 0; attempt < 12; attempt++) {
      const ms = backoffMs(attempt);
      expect(ms).toBeGreaterThanOrEqual(0);
      expect(ms).toBeLessThanOrEqual(Math.min(30_000, 1000 * 2 ** attempt));
    }
  });

  it("sends fewer parts at once when the link is slow, and sizes new uploads to it", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    mockApi();
    const connection = fakeConnection();
    let inFlight = 0;
    const atStart: number[] = [];
    // Every part takes 20 seconds: about 400 KiB/s for the upload as a whole.
    const put = vi.fn(async (_url: string, body: Blob, options?: { onProgress?: (n: number) => void }) => {
      inFlight++;
      atStart.push(inFlight);
      await Promise.resolve();
      vi.setSystemTime(Date.now() + 20_000);
      options?.onProgress?.(body.size);
      inFlight--;
      return { status: 200, etag: `"etag-${body.size}"` };
    });
    const { upload } = makeUpload(bigFile(48 * MIB), put as never, { connection });

    await upload.run();
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(atStart.slice(0, 4)).toEqual([1, 2, 3, 4]);
    expect(atStart.slice(4)).toEqual([1, 1]);
    expect(connection.measuredBps).toBeGreaterThan(SLOW_BPS);

    expect(preferredPartBytes(0)).toBe(8 * MIB);
    expect(preferredPartBytes(100 * 1024)).toBe(5 * MIB);
    expect(preferredPartBytes(20 * MIB)).toBe(16 * MIB);
  });

  it("resumes after a reload from the saved record: sent parts are confirmed, not sent again", async () => {
    const mock = mockApi({ uploadStatuses: ["UPLOADING", "COMPLETED", "VERIFIED"] });
    const store = memoryStore();
    const file = bigFile();
    // The first page sends parts 1 and 2, then closes while part 3 is on its way.
    const firstPut = vi.fn((url: string, body: Blob, options?: { signal?: AbortSignal }) =>
      url.includes("/part/3/") ? hangingPut(options) : okPut(url, body),
    );
    const first = makeUpload(file, firstPut as never, {}, store);
    void first.upload.run();
    await vi.waitFor(async () => expect((await store.get("mp1"))?.unconfirmed).toHaveLength(2));
    const saved = (await store.get("mp1"))!;
    expect(saved.parts).toEqual([]);
    expect(saved.head_sha256).toBe(SHA);

    // A new page: the person chooses the file again, and it is checked.
    expect(await isSameFile(file, saved, async () => SHA)).toBe(true);
    expect(await isSameFile(file, saved, async () => "b".repeat(64))).toBe(false);
    expect(await isSameFile(bigFile(20 * MIB, "other.bin"), saved, async () => SHA)).toBe(false);

    const secondPut = vi.fn(okPut);
    const second = makeUpload(file, secondPut as never, {}, store);
    await second.upload.run(saved);
    await second.upload.settled();

    expect(second.upload.getSnapshot().state).toBe("available");
    expect(secondPut).toHaveBeenCalledTimes(1);
    expect(String(secondPut.mock.calls[0][0])).toContain("/part/3/");
    expect(confirmed(mock)).toEqual([[1, 2], [3]]);
    expect(await store.get("mp1")).toBeUndefined();
  });

  it("sends parts again when the API says they never reached storage", async () => {
    const mock = mockApi({ uploadStatuses: ["UPLOADING", "COMPLETED", "VERIFIED"] });
    let refused = false;
    server.use(
      http.post(`${API}/uploads/:id/parts/confirm`, async ({ request }) => {
        const body = await request.clone().json();
        mock.log.push({ method: "POST", path: "/uploads/mp1/parts/confirm", body });
        if (!refused) {
          refused = true;
          return HttpResponse.json({ error: "invalid part: part 1 is not stored at the provider" }, { status: 400 });
        }
        return HttpResponse.json({ status: "parts_recorded" });
      }),
    );
    const file = bigFile();
    const saved: ResumeRecord = {
      upload_id: "mp1",
      organization_id: "org-1",
      idempotency_key: "key-from-before",
      request,
      file_size: file.size,
      file_last_modified: file.lastModified,
      sha256: SHA,
      part_size: 8 * MIB,
      parts: [],
      unconfirmed: [{ number: 1, etag: '"lost"', size: 8 * MIB }],
      expires_at: "2099-01-01T00:00:00Z",
      saved_at: "2026-11-12T00:00:00Z",
    };
    const put = vi.fn(okPut);
    const { upload } = makeUpload(file, put as never);

    await upload.run(saved);
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(put).toHaveBeenCalledTimes(3);
    expect(confirmed(mock)).toEqual([[1], [1, 2, 3]]);
  });

  it("can be paused and resumed by hand without losing finished parts", async () => {
    const mock = mockApi();
    let hold = true;
    const put = vi.fn((url: string, body: Blob, options?: { signal?: AbortSignal }) =>
      hold && !url.includes("/part/1/") ? hangingPut(options) : okPut(url, body),
    );
    const { upload } = makeUpload(bigFile(), put as never);

    const running = upload.run();
    await vi.waitFor(() => expect(upload.getSnapshot().sentBytes).toBe(8 * MIB));
    upload.pause();
    expect(upload.getSnapshot().paused).toBe("user");

    hold = false;
    await upload.resume();
    await running;
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(putsFor(put, 1)).toHaveLength(1);
    expect(confirmed(mock)).toEqual([[1], [2, 3]]);
  });
});

describe("state changes", () => {
  it("keeps every request on the organization the upload started in", async () => {
    const mock = mockApi();
    let switched = false;
    // The person switches organization while the first part is on its way.
    const put = vi.fn(async (url: string, body: Blob) => {
      if (!switched) {
        switched = true;
        setActiveOrganization("org-2");
      }
      return okPut(url, body);
    });
    const { upload } = makeUpload(bigFile(), put as never);

    await upload.run();
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(mock.log.some((l) => l.path === "/uploads/mp1/complete")).toBe(true);
    expect(new Set(mock.log.map((l) => l.org))).toEqual(new Set(["org-1"]));
  });

  it("checks again after checking stopped while the upload was finishing", async () => {
    const mock = mockApi({ uploadStatuses: ["COMPLETING"] });
    server.use(
      http.post(`${API}/uploads/:id/complete`, () =>
        HttpResponse.json({ upload_id: "mp1", status: "COMPLETING", expires_at: "2099-01-01T00:00:00Z" }, { status: 202 }),
      ),
    );
    const { upload } = makeUpload(bigFile(), okPut as never, { maxPolls: 1 });

    await upload.run();
    await upload.settled();
    expect(upload.getSnapshot()).toMatchObject({ state: "finalizing", pollingStopped: true });

    mock.uploadStatuses = ["VERIFIED"];
    mock.catalog = ["AVAILABLE"];
    await upload.checkNow();

    expect(upload.getSnapshot().state).toBe("available");
  });

  it("does not start a second round of checks while one is still running", async () => {
    const mock = mockApi({ uploadStatuses: ["COMPLETED"] });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const { upload } = makeUpload(new File(["x"], "x.bin"), okPut as never, { sleep: vi.fn(() => gate) });

    await upload.run();
    expect(upload.getSnapshot()).toMatchObject({ state: "verifying", pollingStopped: false });
    const checks = () => mock.log.filter((l) => l.method === "GET").length;
    const before = checks();

    const check = upload.checkNow();
    await Promise.race([check, new Promise((resolve) => setTimeout(resolve, 50))]);
    expect(checks()).toBe(before);

    release();
    await upload.settled();
  });

  it("cannot be cancelled once the completion was sent, and shows the real outcome", async () => {
    const mock = mockApi();
    let answer!: () => void;
    const held = new Promise<void>((resolve) => (answer = resolve));
    server.use(
      http.post(`${API}/uploads/:id/complete`, async () => {
        mock.log.push({ method: "POST", path: "/uploads/sg1/complete" });
        await held;
        return HttpResponse.json({ upload_id: "sg1", status: "COMPLETED", expires_at: "2099-01-01T00:00:00Z" });
      }),
    );
    const { upload } = makeUpload(new File(["x"], "x.bin"), okPut as never);

    const running = upload.run();
    await vi.waitFor(() => expect(mock.log.some((l) => l.path === "/uploads/sg1/complete")).toBe(true));
    expect(upload.getSnapshot().completionSent).toBe(true);
    upload.cancel();
    expect(upload.getSnapshot().state).toBe("finalizing");

    answer();
    await running;
    await upload.settled();
    expect(upload.getSnapshot().state).toBe("available");
    expect(mock.log.some((l) => l.path.endsWith("/abort"))).toBe(false);
  });

  it("starts a retry once, however often Retry is pressed", async () => {
    mockApi({ uploadStatuses: ["UPLOADING", "COMPLETED", "VERIFIED"] });
    let refuse = true;
    const put = vi.fn(async (url: string, body: Blob) => {
      if (refuse) throw new UploadHttpError(400, "InvalidArgument");
      return okPut(url, body);
    });
    const { upload } = makeUpload(bigFile(), put as never);
    await upload.run();
    expect(upload.getSnapshot().state).toBe("failed");

    refuse = false;
    const states: string[] = [];
    upload.subscribe(() => states.push(upload.getSnapshot().state));
    await Promise.all([upload.retry(), upload.retry()]);
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("available");
    expect(states.slice(states.indexOf("hashing"))).not.toContain("queued");
  });

  it("does not bring back the record of an expired session when the checksum finishes late", async () => {
    mockApi({ failPartUrls: () => HttpResponse.json({ error: "multipart upload is not active" }, { status: 409 }) });
    let finishHash!: (sha: string) => void;
    const hash = vi.fn((blob: Blob) => (blob.size <= HEAD_BYTES ? Promise.resolve("head") : new Promise<string>((resolve) => (finishHash = resolve))));
    const { upload, store } = makeUpload(bigFile(), okPut as never, { hash });

    await upload.run();
    expect(upload.getSnapshot().failure?.kind).toBe("session_expired");
    expect(await store.get("mp1")).toBeUndefined();

    finishHash(SHA);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(await store.get("mp1")).toBeUndefined();
  });

  it("is not left paused when the server ends the upload", async () => {
    mockApi({ uploadStatuses: ["COMPLETED"] });
    const { upload } = makeUpload(new File(["x"], "x.bin"), okPut as never);
    // The connection drops just as the server's answer arrives.
    const internals = upload as unknown as { uploadInfo: () => Promise<unknown>; pauseFor: (reason: "offline") => void };
    internals.uploadInfo = async () => {
      internals.pauseFor("offline");
      return { status: "FAILED" };
    };

    await upload.run();
    await upload.settled();

    expect(upload.getSnapshot().state).toBe("failed");
    expect(upload.getSnapshot().paused).toBeUndefined();
  });

  it("sizes new uploads from the speed of all the uploads running together", () => {
    const connection = fakeConnection();
    const first = {};
    const second = {};
    connection.reportSpeed(first, 200 * 1024);
    connection.reportSpeed(second, 200 * 1024);
    expect(connection.measuredBps).toBe(400 * 1024);
    expect(preferredPartBytes(connection.measuredBps)).toBe(8 * MIB);

    // A finished upload stops counting from the next measurement on.
    connection.forgetSpeed(first);
    expect(connection.measuredBps).toBe(400 * 1024);
    connection.reportSpeed(second, 300 * 1024);
    expect(connection.measuredBps).toBe(300 * 1024);
  });
});
