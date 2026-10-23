import { describe, expect, it } from "vitest";
import { cn } from "./cn";

describe("cn", () => {
  it("lets the caller override a default", () => {
    expect(cn("px-4", "px-0")).toBe("px-0");
  });

  it("keeps a custom size and a custom colour together", () => {
    expect(cn("text-md", "text-muted")).toBe("text-md text-muted");
    expect(cn("text-lg", "text-ink", "font-semibold")).toBe("text-lg text-ink font-semibold");
  });

  it("keeps the neomorphic shadows and plain classes", () => {
    expect(cn("neo-sm", "active:shadow-neo-in")).toBe("neo-sm active:shadow-neo-in");
  });
});
