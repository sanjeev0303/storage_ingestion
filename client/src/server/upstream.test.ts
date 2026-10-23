// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { redact } from "./log";
import { hideServerFailure } from "./upstream";

afterEach(() => vi.restoreAllMocks());

describe("hideServerFailure", () => {
  it("logs what the API said and sends the browser only a generic answer", async () => {
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    const upstream = new Response('{"error":"pq: password authentication failed for user storage_app","code":"unavailable"}', {
      status: 503,
      headers: { "x-request-id": "req-9", "retry-after": "5" },
    });

    const res = await hideServerFailure(upstream, "proxy GET datasets");
    const body = await res.json();

    expect(res.status).toBe(503);
    expect(res.headers.get("x-request-id")).toBe("req-9");
    expect(res.headers.get("retry-after")).toBe("5");
    expect(body.code).toBe("unavailable");
    expect(JSON.stringify(body)).not.toContain("pq:");
    expect(logged).toHaveBeenCalledOnce();
    expect(String(logged.mock.calls[0][0])).toContain("password authentication failed");
  });

  it("ignores a code that is not a plain word", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const res = await hideServerFailure(new Response('{"code":"<script>"}', { status: 500 }), "proxy GET x");
    expect((await res.json()).code).toBe("internal");
  });
});

describe("redact", () => {
  it("keeps tokens and passwords out of the log", () => {
    const text = redact('Bearer sat_abc123 and {"password":"hunter2hunter2","access_token":"sat_zzz"} srt_refresh_1');
    expect(text).not.toMatch(/hunter2|sat_abc123|sat_zzz|srt_refresh_1/);
  });
});
