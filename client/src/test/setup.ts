import "@testing-library/jest-dom/vitest";
import { afterAll, afterEach, beforeAll, expect } from "vitest";
import * as axeMatchers from "vitest-axe/matchers";
import { server } from "./server";

expect.extend(axeMatchers);

beforeAll(() => server.listen({ onUnhandledFrame: "error" }));
afterEach(async () => {
  // Files marked "@vitest-environment node" have no DOM to clean up.
  if (typeof window !== "undefined") (await import("@testing-library/react")).cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());

class NoopObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}

// jsdom lacks these; Radix and the layout code touch them.
function installDomShims() {
  window.matchMedia ??= ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
    addListener: () => undefined,
    removeListener: () => undefined,
    dispatchEvent: () => false,
  })) as typeof window.matchMedia;

  globalThis.ResizeObserver ??= NoopObserver as unknown as typeof ResizeObserver;
  globalThis.IntersectionObserver ??= NoopObserver as unknown as typeof IntersectionObserver;
  window.HTMLElement.prototype.scrollIntoView ??= () => undefined;
  window.HTMLElement.prototype.hasPointerCapture ??= () => false;
  window.HTMLElement.prototype.releasePointerCapture ??= () => undefined;
}

if (typeof window !== "undefined") {
  installDomShims();
  // Findings and waits are slow when the whole suite runs at once.
  void import("@testing-library/react").then(({ configure }) => configure({ asyncUtilTimeout: 5000 }));
}
