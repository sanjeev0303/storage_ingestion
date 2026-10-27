import { describe, expect, it, vi } from "vitest";
import { MIB } from "@/lib/upload/parts";
import { UploadHttpError } from "@/lib/upload/put";
import { runDriver, type DriverApi, type DriverEvent, type Putter } from "./driver";
import type { ClaimResponse, IngestionEntry } from "../types";

function entry(position: number, extra: Partial<IngestionEntry> = {}): IngestionEntry {
  return {
    position,
    path: `f${position}.bin`,
    size_bytes: 10,
    content_type: "application/octet-stream",
    required: true,
    strategy: "SINGLE",
    state: "SCHEDULED",
    attempts: 1,
    updated_at: "2026-01-01T00:00:00Z",
    ...extra,
  };
}

const idle: ClaimResponse = { grants: [], limited_by: "no_pending_entries" };

function setup(claims: ClaimResponse[], overrides: Partial<DriverApi> = {}) {
  const queue = [...claims];
  const api: DriverApi = {
    claim: vi.fn(async () => queue.shift() ?? idle),
    uploadUrl: vi.fn(async () => ({ upload_url: "https://store/fresh" })),
    partUrls: vi.fn(async (_p, numbers) => ({
      parts: numbers.map((n: number) => ({ part_number: n, url: `https://store/part/${n}`, expires_at: "" })),
    })),
    confirmParts: vi.fn(async () => undefined),
    heartbeat: vi.fn(async () => undefined),
    complete: vi.fn(async (position) => ({ status: 200, entry: entry(position, { state: "UPLOADED" }) })),
    getEntry: vi.fn(async (position) => entry(position, { state: "UPLOADED" })),
    ...overrides,
  };
  const put: Putter = vi.fn(async () => ({ status: 200, etag: '"etag"' }));
  const waits: number[] = [];
  const events: DriverEvent[] = [];
  const run = (extra: Partial<Parameters<typeof runDriver>[0]> = {}) =>
    runDriver({
      api,
      put,
      getFile: (e) => ({ size: e.size_bytes, slice: () => ({ size: 1 }) }) as unknown as Blob,
      concurrency: 2,
      signal: new AbortController().signal,
      shouldPause: () => false,
      onEvent: (event) => events.push(event),
      sleep: async (ms) => {
        waits.push(ms);
      },
      ...extra,
    });
  return { api, put, waits, events, run };
}

describe("runDriver", () => {
  it("waits retry_after_seconds when a limit holds the claim back, then uploads", async () => {
    const t = setup([
      { grants: [], limited_by: "tenant_slots", retry_after_seconds: 5 },
      { grants: [{ entry: entry(1), upload_url: "https://store/1" }, { entry: entry(2), upload_url: "https://store/2" }] },
    ]);
    const end = await t.run();
    expect(end).toBe("finished");
    expect(t.waits[0]).toBe(5000);
    expect(t.api.claim).toHaveBeenCalledTimes(3);
    expect(t.api.complete).toHaveBeenCalledTimes(2);
  });

  it("sends the entry's content type exactly on a single PUT", async () => {
    const t = setup([{ grants: [{ entry: entry(1, { content_type: "image/x-special" }), upload_url: "https://store/1" }] }]);
    await t.run();
    expect(t.put).toHaveBeenCalledWith("https://store/1", expect.anything(), expect.objectContaining({ headers: { "Content-Type": "image/x-special" } }));
  });

  it("adds x-ms-blob-type for Azure, and after a PUT is refused for lacking it", async () => {
    const azure = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }] }]);
    await azure.run({ azure: true });
    expect(azure.put).toHaveBeenCalledWith("https://store/1", expect.anything(), expect.objectContaining({ headers: expect.objectContaining({ "x-ms-blob-type": "BlockBlob" }) }));

    const learned = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }] }]);
    let calls = 0;
    (learned.put as ReturnType<typeof vi.fn>).mockImplementation(async (_u, _b, options) => {
      calls++;
      if (calls === 1) throw new UploadHttpError(400, "An HTTP header x-ms-blob-type was not specified");
      expect(options.headers["x-ms-blob-type"]).toBe("BlockBlob");
      return { status: 201, etag: null };
    });
    await learned.run();
    expect(calls).toBe(2);
    expect(learned.api.complete).toHaveBeenCalledTimes(1);
  });

  it("keeps going after one file fails", async () => {
    const t = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }, { entry: entry(2), upload_url: "https://store/2" }] }]);
    (t.put as ReturnType<typeof vi.fn>).mockImplementation(async (url: string) => {
      if (url.endsWith("/1")) throw new UploadHttpError(400, "bad request");
      return { status: 200, etag: null };
    });
    await t.run();
    expect(t.events.filter((e) => e.type === "failed")).toHaveLength(1);
    expect(t.events.filter((e) => e.type === "done")).toHaveLength(1);
    expect(t.api.complete).toHaveBeenCalledTimes(1);
  });

  it("uploads multipart in batches of 100 part URLs and confirmations, then completes with every part", async () => {
    const size = 150 * 8 * MIB;
    const t = setup([{ grants: [{ entry: entry(7, { strategy: "MULTIPART", size_bytes: size }) }] }]);
    await t.run({ getFile: () => ({ size, slice: () => ({ size: 8 * MIB }) }) as unknown as Blob });

    const urlCalls = (t.api.partUrls as ReturnType<typeof vi.fn>).mock.calls;
    expect(urlCalls.map((c) => c[1].length)).toEqual([100, 50]);
    const confirms = (t.api.confirmParts as ReturnType<typeof vi.fn>).mock.calls;
    expect(confirms.map((c) => c[1].length)).toEqual([100, 50]);
    expect(confirms[0][1][0]).toMatchObject({ part_number: 1, etag: '"etag"', size_bytes: 8 * MIB });

    const [position, parts] = (t.api.complete as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(position).toBe(7);
    expect(parts).toHaveLength(150);
    expect(parts.map((p: { part_number: number }) => p.part_number)).toEqual(Array.from({ length: 150 }, (_, i) => i + 1));
    expect(Object.keys(parts[0])).toEqual(["part_number", "etag"]);
  });

  it("treats a 202 from complete as confirming and re-polls the entry", async () => {
    const polls = [entry(1, { state: "UPLOADING" }), entry(1, { state: "PROCESSING" })];
    const t = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }] }], {
      complete: vi.fn(async () => ({ status: 202, entry: entry(1, { state: "UPLOADING" }) })),
      getEntry: vi.fn(async () => polls.shift() as IngestionEntry),
    });
    await t.run();
    expect(t.api.getEntry).toHaveBeenCalledTimes(2);
    expect(t.events.find((e) => e.type === "done")).toMatchObject({ state: "PROCESSING" });
  });

  it("sends heartbeats on a schedule while an entry uploads and stops afterwards", async () => {
    // Fake timers: on a busy machine a real 60 ms upload can fit fewer than
    // two 10 ms heartbeats, and the test then fails for nothing.
    vi.useFakeTimers();
    try {
      const t = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }] }]);
      (t.put as ReturnType<typeof vi.fn>).mockImplementation(() => new Promise((resolve) => setTimeout(() => resolve({ status: 200, etag: null }), 60)));
      const running = t.run({ heartbeatMs: 10 });
      await vi.advanceTimersByTimeAsync(60);
      await running;
      const during = (t.api.heartbeat as ReturnType<typeof vi.fn>).mock.calls.length;
      expect(during).toBeGreaterThanOrEqual(2);
      await vi.advanceTimersByTimeAsync(40);
      expect((t.api.heartbeat as ReturnType<typeof vi.fn>).mock.calls.length).toBe(during);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not upload a file whose size differs from the manifest", async () => {
    const t = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }] }]);
    await t.run({ getFile: () => ({ size: 999 }) as unknown as Blob });
    expect(t.put).not.toHaveBeenCalled();
    expect(t.events.some((e) => e.type === "skipped")).toBe(true);
  });

  it("stops instead of claiming the whole job when no files match", async () => {
    const grants = Array.from({ length: 12 }, (_, i) => ({ entry: entry(i + 1), upload_url: "x" }));
    const t = setup([{ grants: grants.slice(0, 6) }, { grants: grants.slice(6) }]);
    const end = await t.run({ getFile: () => undefined, concurrency: 6 });
    expect(end).toBe("unmatched");
  });

  it("stops when the job is not running", async () => {
    const t = setup([{ grants: [], limited_by: "job_not_running" }]);
    expect(await t.run()).toBe("job_not_running");
  });

  it("pauses without claiming more once asked", async () => {
    const t = setup([{ grants: [{ entry: entry(1), upload_url: "https://store/1" }] }]);
    let paused = false;
    (t.api.claim as ReturnType<typeof vi.fn>).mockImplementationOnce(async () => {
      paused = true;
      return { grants: [{ entry: entry(1), upload_url: "https://store/1" }] };
    });
    const end = await t.run({ shouldPause: () => paused });
    expect(end).toBe("paused");
    expect(t.api.claim).toHaveBeenCalledTimes(1);
    expect(t.api.complete).toHaveBeenCalledTimes(1);
  });
});
