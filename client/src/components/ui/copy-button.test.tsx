import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { CopyButton } from "./copy-button";

describe("CopyButton", () => {
  it("leaves no timer behind when it unmounts", async () => {
    vi.useFakeTimers();
    try {
      vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: () => Promise.resolve() } });
      const { unmount } = render(<CopyButton value="abc" />);
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Copy" }));
      });
      expect(screen.getByRole("button", { name: "Copied" })).toBeInTheDocument();
      unmount();
      expect(vi.getTimerCount()).toBe(0);
    } finally {
      vi.unstubAllGlobals();
      vi.useRealTimers();
    }
  });
});
