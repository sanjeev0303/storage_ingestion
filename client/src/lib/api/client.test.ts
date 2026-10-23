import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { api, accountApi, onUnauthenticated, setActiveOrganization } from "./client";
import { ApiError, describeError, isRetryable } from "./errors";

afterEach(() => setActiveOrganization(null));

describe("api client", () => {
  it("sends the active organization and not for account routes", async () => {
    const seen: Record<string, string | null> = {};
    server.use(
      http.get(`${API}/catalog/objects`, ({ request }) => {
        seen.tenant = request.headers.get("x-organization-id");
        return HttpResponse.json({ objects: [] });
      }),
      http.get(`${API}/auth/me`, ({ request }) => {
        seen.account = request.headers.get("x-organization-id");
        return HttpResponse.json({});
      }),
    );
    setActiveOrganization("org-9");
    await api("/catalog/objects");
    await accountApi("/auth/me");
    expect(seen).toEqual({ tenant: "org-9", account: null });
  });

  it("reads {error, code} bodies and Retry-After", async () => {
    server.use(
      http.get(`${API}/x`, () =>
        HttpResponse.json({ error: "slow down", code: "rate_limited" }, { status: 429, headers: { "Retry-After": "7" } }),
      ),
    );
    const error = await api("/x").catch((e) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 429, code: "rate_limited", retryAfter: 7 });
    expect(describeError(error)).toBe("Too many requests. A lot of requests came in a short time. Wait 7 seconds and try again.");
    expect(isRetryable(error)).toBe(true);
  });

  it("copes with the plain-text 400 the backend sends for malformed JSON", async () => {
    server.use(http.post(`${API}/x`, () => new HttpResponse("invalid JSON request\n", { status: 400 })));
    const error = await api("/x", { method: "POST", body: { a: 1 } }).catch((e) => e);
    expect(error).toMatchObject({ status: 400, message: "invalid JSON request" });
    expect(isRetryable(error)).toBe(false);
  });

  it("tells listeners when the session has ended", async () => {
    const listener = vi.fn();
    const off = onUnauthenticated(listener);
    server.use(http.get(`${API}/x`, () => HttpResponse.json({ error: "no", code: "unauthenticated" }, { status: 401 })));
    await api("/x").catch(() => undefined);
    off();
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("reports a network failure without an HTTP status", async () => {
    server.use(http.get(`${API}/x`, () => HttpResponse.error()));
    const error = await api("/x").catch((e) => e);
    expect(error).toMatchObject({ status: 0, code: "network" });
  });
});
