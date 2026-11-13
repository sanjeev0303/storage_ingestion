// @vitest-environment node
import { http, HttpResponse } from "msw";
import { NextRequest } from "next/server";
import { describe, expect, it, vi } from "vitest";
import { server } from "@/test/server";
import { DELETE, GET, POST } from "./route";

const BACKEND = "http://localhost:8080/api/v1";

function call(
  handler: typeof GET,
  path: string,
  init: { method?: string; cookie?: string; headers?: Record<string, string>; body?: string } = {},
) {
  const headers = new Headers(init.headers);
  if (init.cookie) headers.set("cookie", init.cookie);
  const req = new NextRequest(`http://localhost:3100/api/proxy/${path}`, {
    method: init.method ?? "GET",
    headers,
    body: init.body,
  });
  return handler(req, { params: Promise.resolve({ path: path.split("?")[0].split("/") }) });
}

const freshTokens = (label: string) => ({
  access_token: `sat_${label}`,
  access_expires_at: new Date(Date.now() + 900_000).toISOString(),
  refresh_token: `srt_${label}`,
  refresh_expires_at: new Date(Date.now() + 86_400_000).toISOString(),
  session_id: "s1",
});

describe("API proxy", () => {
  it("adds the bearer token from the cookie, forwards the organization, and keeps cookies away from the API", async () => {
    let seen: Headers | undefined;
    server.use(
      http.get(`${BACKEND}/catalog/objects`, ({ request }) => {
        seen = request.headers;
        return HttpResponse.json({ objects: [] });
      }),
    );
    const res = await call(GET, "catalog/objects", {
      cookie: "orb_at=sat_one; orb_rt=srt_one",
      headers: { "x-organization-id": "org-1" },
    });
    expect(res.status).toBe(200);
    expect(seen?.get("authorization")).toBe("Bearer sat_one");
    expect(seen?.get("x-organization-id")).toBe("org-1");
    expect(seen?.get("cookie")).toBeNull();
  });

  it("never lets the browser reach login or refresh, which would hand tokens to script", async () => {
    for (const path of ["auth/login", "auth/refresh"]) {
      const res = await call(POST, path, { method: "POST", body: "{}", headers: { origin: "http://localhost:3100" } });
      expect(res.status).toBe(404);
    }
  });

  it("refuses state-changing requests from another origin", async () => {
    const res = await call(POST, "uploads", {
      method: "POST",
      body: "{}",
      cookie: "orb_at=sat_x; orb_rt=srt_x",
      headers: { origin: "http://evil.example" },
    });
    expect(res.status).toBe(403);
  });

  it("answers 401 without calling the API when there is no session", async () => {
    let called = false;
    server.use(
      http.get(`${BACKEND}/auth/me`, () => {
        called = true;
        return HttpResponse.json({});
      }),
    );
    const res = await call(GET, "auth/me");
    expect(res.status).toBe(401);
    expect(called).toBe(false);
  });

  it("lets token redemption routes through without a session", async () => {
    let auth: string | null = "unset";
    server.use(
      http.post(`${BACKEND}/invitations/accept`, ({ request }) => {
        auth = request.headers.get("authorization");
        return HttpResponse.json({}, { status: 201 });
      }),
    );
    const res = await call(POST, "invitations/accept", {
      method: "POST",
      body: JSON.stringify({ token: "sit_x", password: "long enough password" }),
      headers: { origin: "http://localhost:3100", "content-type": "application/json" },
    });
    expect(res.status).toBe(201);
    expect(auth).toBeNull();
  });

  it("refreshes when only the refresh cookie is left, then sets the new cookies", async () => {
    let bearer: string | null = null;
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => HttpResponse.json(freshTokens("renewed"))),
      http.get(`${BACKEND}/catalog/objects`, ({ request }) => {
        bearer = request.headers.get("authorization");
        return HttpResponse.json({ objects: [] });
      }),
    );
    const res = await call(GET, "catalog/objects", { cookie: "orb_rt=srt_expired_access" });
    expect(res.status).toBe(200);
    expect(bearer).toBe("Bearer sat_renewed");
    const cookies = res.headers.getSetCookie().join("\n");
    expect(cookies).toContain("orb_at=sat_renewed");
    expect(cookies).toContain("orb_rt=srt_renewed");
    expect(cookies).toMatch(/HttpOnly/i);
    expect(cookies).toMatch(/SameSite=strict/i);
  });

  it("retries once after a 401 by refreshing, and replays a request body", async () => {
    const received: string[] = [];
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => HttpResponse.json(freshTokens("second"))),
      http.post(`${BACKEND}/uploads`, async ({ request }) => {
        if (request.headers.get("authorization") === "Bearer sat_stale") {
          return HttpResponse.json({ error: "no", code: "unauthenticated" }, { status: 401 });
        }
        received.push(await request.text());
        return HttpResponse.json({ upload_id: "u1" }, { status: 201 });
      }),
    );
    const res = await call(POST, "uploads", {
      method: "POST",
      body: '{"file_name":"a.bin"}',
      cookie: "orb_at=sat_stale; orb_rt=srt_for_retry",
      headers: { origin: "http://localhost:3100", "content-type": "application/json" },
    });
    expect(res.status).toBe(201);
    expect(received).toEqual(['{"file_name":"a.bin"}']);
    expect(res.headers.getSetCookie().join("\n")).toContain("orb_at=sat_second");
  });

  it("ends the session when the refresh token is refused", async () => {
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => HttpResponse.json({ error: "no", code: "refresh_reused" }, { status: 401 })),
    );
    const res = await call(GET, "catalog/objects", { cookie: "orb_rt=srt_revoked" });
    expect(res.status).toBe(401);
    const cookies = res.headers.getSetCookie().join("\n");
    expect(cookies).toContain("orb_at=;");
    expect(cookies).toContain("orb_rt=;");
  });

  it("answers 401, not a broken response, when a revoked session cannot be refreshed", async () => {
    server.use(
      http.get(`${BACKEND}/auth/sessions`, () => HttpResponse.json({ error: "session revoked", code: "unauthenticated" }, { status: 401 })),
      http.post(`${BACKEND}/auth/refresh`, () => HttpResponse.json({ error: "no", code: "unauthenticated" }, { status: 401 })),
    );
    const res = await call(GET, "auth/sessions", { cookie: "orb_at=sat_revoked; orb_rt=srt_revoked" });
    expect(res.status).toBe(401);
    expect(await res.json()).toMatchObject({ code: "unauthenticated" });
    expect(res.headers.getSetCookie().join(" ")).toContain("orb_rt=;");
  });

  it("passes download headers and the body through", async () => {
    server.use(
      http.get(`${BACKEND}/assets/a1/content`, () =>
        new HttpResponse("hello", {
          status: 206,
          headers: {
            "content-range": "bytes 0-4/10",
            etag: '"abc"',
            "content-disposition": 'attachment; filename="a.txt"',
            "set-cookie": "leak=1",
          },
        }),
      ),
    );
    const res = await call(GET, "assets/a1/content", {
      cookie: "orb_at=sat_dl; orb_rt=srt_dl",
      headers: { range: "bytes=0-4" },
    });
    expect(res.status).toBe(206);
    expect(res.headers.get("content-range")).toBe("bytes 0-4/10");
    expect(res.headers.get("etag")).toBe('"abc"');
    expect(res.headers.get("set-cookie")).toBeNull();
    expect(await res.text()).toBe("hello");
  });

  it("returns 204 responses without a body", async () => {
    server.use(http.delete(`${BACKEND}/auth/sessions/s9`, () => new HttpResponse(null, { status: 204 })));
    const res = await call(DELETE, "auth/sessions/s9", {
      method: "DELETE",
      cookie: "orb_at=sat_d; orb_rt=srt_d",
      headers: { origin: "http://localhost:3100" },
    });
    expect(res.status).toBe(204);
  });

  it("sends the API one caller address it can trust, never the caller's own forwarded entries", async () => {
    let forwarded: string | null = "unset";
    server.use(
      http.get(`${BACKEND}/catalog/objects`, ({ request }) => {
        forwarded = request.headers.get("x-forwarded-for");
        return HttpResponse.json({ objects: [] });
      }),
    );
    // With no proxy in front, the last entry is the connection's address;
    // anything the caller wrote before it is dropped.
    await call(GET, "catalog/objects", {
      cookie: "orb_at=sat_ip; orb_rt=srt_ip",
      headers: { "x-forwarded-for": "10.9.9.9, 203.0.113.5" },
    });
    expect(forwarded).toBe("203.0.113.5");
    // Something that is not an address is not forwarded at all.
    await call(GET, "catalog/objects", {
      cookie: "orb_at=sat_ip; orb_rt=srt_ip",
      headers: { "x-forwarded-for": "not-an-address" },
    });
    expect(forwarded).toBeNull();
  });

  it("passes the caller's address on the refresh it makes", async () => {
    let refreshFrom: string | null = null;
    server.use(
      http.post(`${BACKEND}/auth/refresh`, ({ request }) => {
        refreshFrom = request.headers.get("x-forwarded-for");
        return HttpResponse.json(freshTokens("ipr"));
      }),
      http.get(`${BACKEND}/catalog/objects`, () => HttpResponse.json({ objects: [] })),
    );
    const res = await call(GET, "catalog/objects", { cookie: "orb_rt=srt_ip_refresh", headers: { "x-forwarded-for": "198.51.100.7" } });
    expect(res.status).toBe(200);
    expect(refreshFrom).toBe("198.51.100.7");
  });

  it("refuses a body without a session before reading it, and caps the size of any body", async () => {
    let reached = false;
    server.use(
      http.post(`${BACKEND}/uploads`, () => {
        reached = true;
        return HttpResponse.json({});
      }),
    );
    const anonymous = await call(POST, "uploads", { method: "POST", body: "x".repeat(1000), headers: { origin: "http://localhost:3100" } });
    expect(anonymous.status).toBe(401);

    const tooLarge = await call(POST, "uploads", {
      method: "POST",
      cookie: "orb_at=sat_big; orb_rt=srt_big",
      headers: { origin: "http://localhost:3100", "content-length": String(18 * 1024 * 1024) },
      body: "{}",
    });
    expect(tooLarge.status).toBe(413);
    expect(reached).toBe(false);
  });

  it("keeps an encoded ? in a path segment encoded, so it cannot become a query", async () => {
    let seenPath = "";
    server.use(
      http.get(`${BACKEND}/*`, ({ request }) => {
        seenPath = new URL(request.url).pathname + new URL(request.url).search;
        return HttpResponse.json({});
      }),
    );
    const req = new NextRequest("http://localhost:3100/api/proxy/auth/login%3Fx", {
      headers: { cookie: "orb_at=sat_q; orb_rt=srt_q" },
    });
    await GET(req, { params: Promise.resolve({ path: ["auth", "login?x"] }) });
    expect(seenPath).toBe("/api/v1/auth/login%3Fx");
  });
  it("renews before reading the body, so a slow body never presents a stale refresh token", async () => {
    const order: string[] = [];
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => {
        order.push("refresh");
        return HttpResponse.json(freshTokens("early"));
      }),
      http.post(`${BACKEND}/uploads`, () => HttpResponse.json({}, { status: 201 })),
    );
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        order.push("body");
        controller.enqueue(new TextEncoder().encode("{}"));
        controller.close();
      },
    }, { highWaterMark: 0 });
    const req = new NextRequest("http://localhost:3100/api/proxy/uploads", {
      method: "POST",
      headers: { cookie: "orb_rt=srt_slow_body", origin: "http://localhost:3100" },
      body,
      duplex: "half",
    } as ConstructorParameters<typeof NextRequest>[1]);
    const res = await POST(req, { params: Promise.resolve({ path: ["uploads"] }) });
    expect(res.status).toBe(201);
    expect(order[0]).toBe("refresh");
  });

  it("asks for a retry instead of presenting a refresh token that arrived too long ago", async () => {
    let refreshed = false;
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      server.use(
        http.post(`${BACKEND}/auth/refresh`, () => {
          refreshed = true;
          return HttpResponse.json(freshTokens("late"));
        }),
        http.post(`${BACKEND}/uploads`, () => {
          // The request took longer than the window in which a concurrent
          // rotation's answer is still cached.
          vi.setSystemTime(Date.now() + 6_000);
          return HttpResponse.json({ error: "no", code: "unauthenticated" }, { status: 401 });
        }),
      );
      const res = await call(POST, "uploads", {
        method: "POST",
        body: "{}",
        cookie: "orb_at=sat_expired_in_flight; orb_rt=srt_late",
        headers: { origin: "http://localhost:3100" },
      });
      expect(res.status).toBe(503);
      expect(refreshed).toBe(false);
      expect(res.headers.getSetCookie()).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("answers 503 and does not present the refresh token again after a refresh got no answer", async () => {
    let calls = 0;
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => {
        calls++;
        return new HttpResponse(null, { status: 502 });
      }),
    );
    const first = await call(GET, "catalog/objects", { cookie: "orb_rt=srt_lost_answer" });
    const second = await call(GET, "catalog/objects", { cookie: "orb_rt=srt_lost_answer" });
    expect(first.status).toBe(503);
    expect(second.status).toBe(503);
    expect(calls).toBe(1);
    expect(first.headers.getSetCookie()).toEqual([]);
  });

  it("keeps the session on a 401 that is not about the session, such as a wrong current password", async () => {
    let refreshed = false;
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => {
        refreshed = true;
        return HttpResponse.json(freshTokens("pw"));
      }),
      http.post(`${BACKEND}/auth/password`, () =>
        HttpResponse.json({ error: "invalid credentials", code: "invalid_credentials" }, { status: 401 }),
      ),
    );
    const res = await call(POST, "auth/password", {
      method: "POST",
      body: "{}",
      cookie: "orb_at=sat_pw; orb_rt=srt_pw",
      headers: { origin: "http://localhost:3100" },
    });
    expect(res.status).toBe(401);
    expect(await res.json()).toMatchObject({ code: "invalid_credentials" });
    expect(refreshed).toBe(false);
    expect(res.headers.getSetCookie()).toEqual([]);
  });

  it("clears the session, and sets no new cookies, when the API refuses even the renewed token", async () => {
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => HttpResponse.json(freshTokens("refused"))),
      http.get(`${BACKEND}/catalog/objects`, () =>
        HttpResponse.json({ error: "session revoked", code: "unauthenticated" }, { status: 401 }),
      ),
    );
    const res = await call(GET, "catalog/objects", { cookie: "orb_at=sat_old_r; orb_rt=srt_refused" });
    expect(res.status).toBe(401);
    const cookies = res.headers.getSetCookie().join(" ");
    expect(cookies).not.toContain("sat_refused");
    expect(cookies).toContain("orb_at=;");
    expect(cookies).toContain("orb_rt=;");
  });
});
