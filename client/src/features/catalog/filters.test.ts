import { describe, expect, it } from "vitest";
import { emptyFilters, filtersToParams, parseFilters, toApiParams } from "./filters";
import { buildPatch, validateKey, validateValue } from "./metadata";
import { downloadHref, planPreview } from "./preview";

describe("filters", () => {
  it("round-trips through the query string", () => {
    const filters = {
      ...emptyFilters,
      dataset: "images",
      prefix: "2026/",
      min_size: "10",
      order: "updated" as const,
      include_unpublished: true,
      metadata: [{ key: "stage", value: "final" }],
    };
    const qs = filtersToParams(filters).toString();
    expect(qs).toBe("dataset=images&prefix=2026%2F&min_size=10&metadata.stage=final&include=unpublished&order=updated");
    expect(parseFilters(new URLSearchParams(qs))).toEqual(filters);
  });

  it("tells root only (empty dataset) from all datasets", () => {
    expect(filtersToParams(emptyFilters).has("dataset")).toBe(false);
    expect(filtersToParams({ ...emptyFilters, dataset: "" }).toString()).toBe("dataset=");
    expect(parseFilters(new URLSearchParams("dataset=")).dataset).toBe("");
    expect(parseFilters(new URLSearchParams("")).dataset).toBeUndefined();
  });

  it("turns plain dates into RFC 3339 for the API only", () => {
    const api = toApiParams(new URLSearchParams("created_after=2026-03-01"));
    expect(api.get("created_after")).toBe("2026-03-01T00:00:00Z");
  });

  it("keeps at most four metadata filters", () => {
    const qs = "metadata.a=1&metadata.b=2&metadata.c=3&metadata.d=4&metadata.e=5";
    expect(parseFilters(new URLSearchParams(qs)).metadata).toHaveLength(4);
  });
});

describe("metadata rules", () => {
  it("checks keys", () => {
    expect(validateKey("stage")).toBeNull();
    expect(validateKey("")).not.toBeNull();
    expect(validateKey("has space")).not.toBeNull();
    expect(validateKey("a".repeat(65))).not.toBeNull();
    expect(validateKey("system.owner")).toMatch(/reserved/);
  });

  it("limits values by bytes, not characters", () => {
    expect(validateValue("a".repeat(1024))).toBeNull();
    expect(validateValue("é".repeat(513))).not.toBeNull();
  });

  it("builds a merge patch with null for removed keys", () => {
    expect(buildPatch({ a: "1", b: "2", c: "3" }, { a: "1", b: "x", d: "4" })).toEqual({ b: "x", c: null, d: "4" });
  });
});

describe("preview and download rules", () => {
  it("previews only safe types within size limits", () => {
    expect(planPreview("image/png", 1000, "AVAILABLE").kind).toBe("image");
    expect(planPreview("application/json; charset=utf-8", 1000, "AVAILABLE").kind).toBe("text");
    expect(planPreview("image/svg+xml", 10, "AVAILABLE").kind).toBe("none");
    expect(planPreview("text/html", 10, "AVAILABLE").kind).toBe("none");
    expect(planPreview("image/png", 21 * 1024 * 1024, "AVAILABLE").kind).toBe("none");
    expect(planPreview("text/plain", 300 * 1024, "AVAILABLE").kind).toBe("none");
    expect(planPreview("image/png", 10, "SOFT_DELETED").kind).toBe("none");
  });

  it("routes token links through the proxy and leaves presigned URLs alone", () => {
    expect(downloadHref("/api/v1/downloads/abc", "token")).toBe("/api/proxy/downloads/abc");
    expect(downloadHref("https://s3.example/o?sig=1", "storage")).toBe("https://s3.example/o?sig=1");
    expect(downloadHref("https://cdn.example/o?sig=1", "cdn")).toBe("https://cdn.example/o?sig=1");
  });
});
