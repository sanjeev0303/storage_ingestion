import { describe, expect, it } from "vitest";
import { Blob as NodeBlob } from "node:buffer";
import { MAX_SAMPLES, MAX_TRACKED_PATHS, exceededLimits, preflightManifest } from "./preflight";

const sum = "sha256:" + "a".repeat(64);
const line = (extra: Record<string, unknown> = {}) =>
  JSON.stringify({ path: "a/b.jpg", size: 10, content_type: "image/jpeg", checksum: sum, ...extra });

const asFile = (text: string) => new NodeBlob([text]) as unknown as Blob;

describe("preflightManifest", () => {
  it("counts lines, bytes and blank lines from a stream", async () => {
    const text = [line({ path: "one" }), "", line({ path: "two", size: 5 }), line({ path: "three", size: 7 })].join("\n");
    const report = await preflightManifest(asFile(text));
    expect(report.lines).toBe(3);
    expect(report.blankLines).toBe(1);
    expect(report.problemLines).toBe(0);
    expect(report.totalEntryBytes).toBe(22);
  });

  it("flags bad JSON, missing keys and a bad checksum with the line number", async () => {
    const text = [
      "{not json",
      JSON.stringify({ size: 1, content_type: "a/b", checksum: sum }),
      JSON.stringify({ path: "x", size_bytes: 3, content_type: "a/b", checksum: sum }),
      JSON.stringify({ path: "y", size: 3, checksum: sum }),
      JSON.stringify({ path: "z", size: 3, content_type: "a/b" }),
      JSON.stringify({ path: "w", size: 3, content_type: "a/b", checksum: "sha256:abc" }),
      JSON.stringify({ path: "v", size: 3, content_type: "a/b", checksum: "md5:" + "a".repeat(32) }),
      line({ path: "fine" }),
    ].join("\n");
    const report = await preflightManifest(asFile(text));
    expect(report.problemLines).toBe(7);
    expect(report.samples.map((s) => s.line)).toEqual([1, 2, 3, 4, 5, 6, 7]);
    expect(report.samples[2].problem).toMatch(/size_bytes/);
    expect(report.samples[4].problem).toMatch(/checksum_required/);
    expect(report.samples[5].problem).toMatch(/invalid_checksum/);
  });

  it("keeps only the first 50 error samples but counts every problem", async () => {
    const text = Array.from({ length: 120 }, () => "nope").join("\n");
    const report = await preflightManifest(asFile(text));
    expect(report.problemLines).toBe(120);
    expect(report.samples).toHaveLength(MAX_SAMPLES);
    expect(report.samples.at(-1)?.line).toBe(MAX_SAMPLES);
  });

  it("finds duplicate paths without regard to case or ./ and // spelling", async () => {
    const text = [line({ path: "Photos/Cat.jpg" }), line({ path: "photos//./cat.jpg" }), line({ path: "other" })].join("\n");
    const report = await preflightManifest(asFile(text));
    expect(report.duplicatePaths).toBe(1);
    expect(report.samples[0]).toMatchObject({ line: 2 });
    expect(report.duplicatesFullyChecked).toBe(true);
  });

  it("flags a line over 16384 bytes without keeping it, and carries on", async () => {
    const long = line({ path: "p", metadata: { note: "x".repeat(20000) } });
    const text = [line({ path: "first" }), long, line({ path: "last" })].join("\n");
    const report = await preflightManifest(asFile(text));
    expect(report.lines).toBe(3);
    expect(report.problemLines).toBe(1);
    expect(report.samples[0]).toMatchObject({ line: 2 });
    expect(report.samples[0].problem).toMatch(/line_too_long/);
  });

  it("handles CRLF endings and a final line with no newline", async () => {
    const report = await preflightManifest(asFile(`${line({ path: "a" })}\r\n${line({ path: "b" })}`));
    expect(report.lines).toBe(2);
    expect(report.problemLines).toBe(0);
  });

  it("says duplicates are only checked up to a bound", () => {
    expect(MAX_TRACKED_PATHS).toBe(200_000);
  });
});

describe("exceededLimits", () => {
  const report = { fileBytes: 500, lines: 20 } as Parameters<typeof exceededLimits>[0];

  it("compares lines and bytes with the quota, and treats 0 as no limit", () => {
    expect(exceededLimits(report, { maxEntries: 10, maxBytes: 1000 })).toEqual({ entries: true, bytes: false });
    expect(exceededLimits(report, { maxEntries: 0, maxBytes: 100 })).toEqual({ entries: false, bytes: true });
    expect(exceededLimits(report, {})).toEqual({ entries: false, bytes: false });
  });
});
