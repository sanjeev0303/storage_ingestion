import { Blob as NodeBlob } from "node:buffer";
import { describe, expect, it, vi } from "vitest";
import { choosePartSize, MAX_PARTS, MIB, planParts, runPool } from "./parts";
import { isTransientPutError, UploadHttpError, UploadNetworkError } from "./put";
import { withRetry } from "./retry";
import { sha256Blob } from "./sha256";

describe("part planning", () => {
  it("never goes under 5 MiB or over 10,000 parts", () => {
    expect(choosePartSize(10 * MIB)).toBe(8 * MIB);
    expect(choosePartSize(1)).toBe(8 * MIB);
    const huge = 5 * 1024 * 1024 * 1024 * 1024;
    const size = choosePartSize(huge);
    expect(Math.ceil(huge / size)).toBeLessThanOrEqual(MAX_PARTS);
  });

  it("covers the file exactly with a short last part", () => {
    const parts = planParts(20 * MIB + 5, 8 * MIB);
    expect(parts.map((p) => p.size)).toEqual([8 * MIB, 8 * MIB, 4 * MIB + 5]);
    expect(parts.at(-1)?.end).toBe(20 * MIB + 5);
    expect(parts.map((p) => p.number)).toEqual([1, 2, 3]);
  });
});

describe("runPool", () => {
  it("keeps concurrency bounded", async () => {
    let running = 0;
    let peak = 0;
    await runPool(Array.from({ length: 12 }), 3, async () => {
      running++;
      peak = Math.max(peak, running);
      await new Promise((r) => setTimeout(r, 2));
      running--;
    });
    expect(peak).toBe(3);
  });

  it("stops starting work after the first failure and rethrows it", async () => {
    const started: number[] = [];
    const error = await runPool([1, 2, 3, 4, 5, 6], 1, async (n) => {
      started.push(n);
      if (n === 2) throw new Error("boom");
    }).catch((e) => e);
    expect(error).toEqual(new Error("boom"));
    expect(started).toEqual([1, 2]);
  });
});

describe("withRetry", () => {
  const sleep = vi.fn(async () => undefined);

  it("retries transient failures then succeeds", async () => {
    let calls = 0;
    const result = await withRetry(
      async () => {
        calls++;
        if (calls < 3) throw new UploadNetworkError();
        return "ok";
      },
      { shouldRetry: isTransientPutError, sleep },
    );
    expect(result).toBe("ok");
    expect(calls).toBe(3);
  });

  it("does not retry a rejected signature", async () => {
    let calls = 0;
    await expect(
      withRetry(
        async () => {
          calls++;
          throw new UploadHttpError(403, "SignatureDoesNotMatch");
        },
        { shouldRetry: isTransientPutError, sleep },
      ),
    ).rejects.toBeInstanceOf(UploadHttpError);
    expect(calls).toBe(1);
  });

  it("stops at once when aborted", async () => {
    const controller = new AbortController();
    controller.abort();
    const run = vi.fn();
    await expect(withRetry(run, { signal: controller.signal })).rejects.toMatchObject({ name: "AbortError" });
    expect(run).not.toHaveBeenCalled();
  });
});

describe("sha256Blob", () => {
  it("matches the known digest of 'abc' and reports progress", async () => {
    const progress: number[] = [];
    // jsdom's Blob has no stream(); browsers and Node's do.
    const blob = new NodeBlob(["abc"]) as unknown as Blob;
    const hex = await sha256Blob(blob, { onProgress: (n) => progress.push(n) });
    expect(hex).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    expect(progress.at(-1)).toBe(3);
  });
});
