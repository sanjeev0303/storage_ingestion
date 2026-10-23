import { describe, expect, it } from "vitest";
import { Blob as NodeBlob } from "node:buffer";
import { http, HttpResponse } from "msw";
import { ApiError } from "@/lib/api/errors";
import { API, server } from "@/test/server";
import { completeEntry, createJob, listEntries, putManifestInline, retryEntries } from "./api";
import { buildRetryBody } from "./lib/retry-body";
import { exportFailuresCsv, csvCell } from "./lib/export";
import { INLINE_LIMIT_BYTES, manifestMode, submitManifest } from "./lib/manifest";

const job = { id: "job-1", state: "PENDING_VALIDATION" };

describe("job and entry requests", () => {
  it("sends the state filter as a comma list and the cursor as a number", async () => {
    let url: URL | undefined;
    server.use(
      http.get(`${API}/ingestion/jobs/job-1/entries`, ({ request }) => {
        url = new URL(request.url);
        return HttpResponse.json({ entries: [], next_after: 40 });
      }),
    );
    const page = await listEntries("job-1", "PENDING,FAILED", 20);
    expect(url?.searchParams.get("state")).toBe("PENDING,FAILED");
    expect(url?.searchParams.get("after")).toBe("20");
    expect(page.next_after).toBe(40);
  });

  it("always sends a body with retry, either positions or all", async () => {
    const bodies: unknown[] = [];
    server.use(
      http.post(`${API}/ingestion/jobs/job-1/retry`, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ retried: 2, requeued: 1 });
      }),
    );
    expect(await retryEntries("job-1", { all: true })).toEqual({ retried: 2, requeued: 1 });
    await retryEntries("job-1", { positions: [3, 4] });
    expect(bodies).toEqual([{ all: true }, { positions: [3, 4] }]);
  });

  it("builds the retry body from the box: empty means all, a list means those entries", () => {
    expect(buildRetryBody("")).toEqual({ all: true });
    expect(buildRetryBody("3, 4 4,9")).toEqual({ positions: [3, 4, 9] });
    expect(buildRetryBody("x")).toHaveProperty("error");
    expect(buildRetryBody("0")).toHaveProperty("error");
    expect(buildRetryBody(Array.from({ length: 1001 }, (_, i) => i + 1).join(","))).toHaveProperty("error");
  });

  it("tells a new job (201) from one that already existed (200)", async () => {
    server.use(
      http.post(`${API}/ingestion/jobs`, () => HttpResponse.json(job, { status: 200 })),
    );
    const body = { dataset_id: "d", idempotency_key: "k", max_concurrency: 0 };
    expect((await createJob(body)).existed).toBe(true);
    server.use(http.post(`${API}/ingestion/jobs`, () => HttpResponse.json(job, { status: 201 })));
    expect((await createJob(body)).existed).toBe(false);
  });

  it("reports the status of complete so a 202 can be re-polled", async () => {
    server.use(http.post(`${API}/ingestion/jobs/job-1/entries/5/complete`, () => HttpResponse.json({ position: 5, state: "UPLOADING" }, { status: 202 })));
    expect((await completeEntry("job-1", 5)).status).toBe(202);
  });
});

describe("manifest submission", () => {
  const line = JSON.stringify({ path: "a", size: 1, content_type: "a/b", checksum: "sha256:" + "0".repeat(64) });
  const file = new NodeBlob([line + "\n"]) as unknown as File;

  it("decides inline or staged by the 16 MiB line", () => {
    expect(manifestMode(INLINE_LIMIT_BYTES)).toBe("inline");
    expect(manifestMode(INLINE_LIMIT_BYTES + 1)).toBe("staged");
  });

  it("sends a small manifest inline with its checksum header", async () => {
    let header: string | null = null;
    server.use(
      http.put(`${API}/ingestion/jobs/job-1/manifest`, ({ request }) => {
        header = request.headers.get("x-manifest-sha256");
        return HttpResponse.json({ lines: 1, accepted: 1, invalid: 0, duplicates: 0, replayed: 0, complete: true });
      }),
    );
    const result = await submitManifest("job-1", file);
    expect(result.mode).toBe("inline");
    expect(header).toMatch(/^[0-9a-f]{64}$/);
  });

  it("surfaces the report that comes with a 413", async () => {
    server.use(
      http.put(`${API}/ingestion/jobs/job-1/manifest`, () =>
        HttpResponse.json({ error: "too many lines", report: { lines: 9, accepted: 0, invalid: 0, duplicates: 0, replayed: 0, complete: false } }, { status: 413 }),
      ),
    );
    const error = await putManifestInline("job-1", file).catch((e) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect((error.body as { report: { lines: number } }).report.lines).toBe(9);
  });
});

describe("failure export", () => {
  it("pages through failures and writes a CSV, guarding formula cells", async () => {
    const pages = [
      { entries: [{ position: 1, path: "=cmd", state: "FAILED", error_code: "size_mismatch", error_detail: 'said "no", twice', attempts: 2, size_bytes: 5, updated_at: "t" }], next_after: 1 },
      { entries: [{ position: 2, path: "b", state: "INVALID", error_code: "invalid_json", attempts: 0, size_bytes: 0, updated_at: "t" }] },
    ];
    const seen: (number | undefined)[] = [];
    const result = await exportFailuresCsv(async (after) => {
      seen.push(after);
      return pages.shift() as never;
    });
    const text = await (result.blob as Blob).text();
    expect(seen).toEqual([undefined, 1]);
    expect(result.rows).toBe(2);
    expect(text.split("\n")[1]).toBe(`1,'=cmd,FAILED,size_mismatch,"said ""no"", twice",2,5,t`);
    expect(csvCell("plain")).toBe("plain");
  });

  it("stops at the cap", async () => {
    const entries = Array.from({ length: 5 }, (_, i) => ({ position: i + 1 }));
    const result = await exportFailuresCsv(async () => ({ entries: entries as never, next_after: 99 }), { cap: 3 });
    expect(result.rows).toBe(3);
    expect(result.capped).toBe(true);
  });
});
