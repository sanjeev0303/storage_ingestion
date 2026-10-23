import { describe, expect, it } from "vitest";
import { safeNext } from "./redirect";

describe("safeNext", () => {
  it("keeps console paths", () => {
    expect(safeNext("/app/datasets/abc?x=1")).toBe("/app/datasets/abc?x=1");
  });

  it.each(["//evil.example", "https://evil.example/app", "/login", "/app\\..\\x", "javascript:alert(1)", "", null, undefined])(
    "falls back for %s",
    (value) => {
      expect(safeNext(value as string | null)).toBe("/app/overview");
    },
  );
});
