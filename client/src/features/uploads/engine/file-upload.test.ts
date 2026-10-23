import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { setActiveOrganization } from "@/lib/api/client";
import { MIB } from "@/lib/upload/parts";
import { UploadHttpError, UploadNetworkError } from "@/lib/upload/put";
import { abortError } from "@/lib/upload/sha256";
import { API, server } from "@/test/server";
import type { ResumeRecord } from "../types";
import { FileUpload } from "./file-upload";
import { memoryStore } from "./store";

const SHA = "a".repeat(64);
const request = { file_name: "big.bin", content_type: "application/octet-stream" };

type Log = { method: string; path: string; body?: unknown }[];

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
    mock.log.push({ method: request.method, path: url.pathname.replace("/api/proxy", ""), body: text ? JSON.parse(text) : undefined });
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
      return HttpResponse.json({ upload_id: "mp1", status, file_name: "big.bin", size_bytes: 1, expires_at: "2099-01-01T00:00:00Z", multipart: true });
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

function makeUpload(file: File, put: unknown, extra = {}, store = memoryStore()) {
  const sleep = vi.fn(async () => undefined);
  const upload = new FileUpload(file, request, {
    put: put as never,
    hash: async () => SHA,
    sleep,
    store,
    pollMs: 0,
    ...extra,
  });
  return { upload, sleep, store };
}

const okPut = async (_url: string, body: Blob) => ({ status: 200, etag: `"etag-${body.size}"` });

beforeEach(() => setActiveOrganization("org-1"));

describe("multipart upload", () => {
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

    const snap = upload.getSnapshot();
    expect(snap.state).toBe("available");
    expect(snap.asset).toMatchObject({ objectId: "obj1", status: "AVAILABLE" });
    expect(put.mock.calls.filter(([url]) => String(url).includes("/part/2/"))).toHaveLength(2);

    const start = mock.log.find((l) => l.path === "/uploads/multipart");
    expect(start?.body).toMatchObject({ checksum_sha256: SHA, size_bytes: 20 * MIB, idempotency_key: expect.any(String) });

    const complete = mock.log.find((l) => l.path === "/uploads/mp1/complete");
    expect(complete?.body).toEqual({
      parts: [
        { part_number: 1, etag: `"etag-${8 * MIB}"` },
        { part_number: 2, etag: `"etag-${8 * MIB}"` },
        { part_number: 3, etag: `"etag-${4 * MIB}"` },
      ],
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
    expect(upload.getSnapshot().state).toBe("failed");
    expect(upload.getSnapshot().failure?.kind).toBe("session_expired");

    expired = false;
    await upload.restart();

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
    const upload = new FileUpload(file, { file_name: "data.csv", content_type: "text/csv" }, { put: put as never, hash: async () => SHA, sleep, store: memoryStore(), pollMs: 0 });

    server.use(http.get(`${API}/catalog/objects/:id/versions`, () => HttpResponse.json({ versions: [{ version: 1, asset_id: "as1", status: "AVAILABLE", checksum_sha256: SHA, size_bytes: 8 }] })));
    server.use(http.get(`${API}/catalog/lookup`, () => HttpResponse.json({ id: "obj1", dataset: "", path: "data.csv", versions: 1 })));
    mock.uploadStatuses = ["VERIFIED"];
    server.use(http.post(`${API}/uploads/sg1/complete`, async ({ request }) => {
      mock.log.push({ method: "POST", path: "/uploads/sg1/complete", body: await request.text() });
      return HttpResponse.json({ upload_id: "sg1", status: "COMPLETED" });
    }));

    await upload.run();

    expect(put).toHaveBeenCalledWith("https://store.test/single", file, expect.objectContaining({ headers: { "x-ms-blob-type": "BlockBlob", "Content-Type": "text/csv" } }));
    expect(mock.log.find((l) => l.path === "/uploads/sg1/complete")?.body).toBe("");
    expect(mock.log.find((l) => l.path === "/uploads")?.body).toMatchObject({ checksum_sha256: SHA, content_type: "text/csv" });
    expect(upload.getSnapshot().mode).toBe("single");
  });

  it("shows a rejected asset with the reason from the catalog", async () => {
    mockApi({ catalog: ["REJECTED"], uploadStatuses: ["VERIFIED"] });
    const file = new File(["x"], "x.bin");
    const upload = new FileUpload(file, { file_name: "big.bin", content_type: "application/octet-stream" }, { put: (async () => ({ status: 200, etag: null })) as never, hash: async () => SHA, sleep: async () => undefined, store: memoryStore(), pollMs: 0 });

    await upload.run();

    expect(upload.getSnapshot().state).toBe("rejected");
    expect(upload.getSnapshot().asset?.rejectionReason).toBe("contains a virus");
  });
});
