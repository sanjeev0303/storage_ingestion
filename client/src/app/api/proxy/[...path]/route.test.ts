// @vitest-environment node
import { http, HttpResponse } from "msw";
import { NextRequest } from "next/server";
import { describe, expect, it } from "vitest";
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
      cookie: "osp_at=sat_one; osp_rt=srt_one",
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
      cookie: "osp_at=sat_x; osp_rt=srt_x",
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
    const res = await call(GET, "catalog/objects", { cookie: "osp_rt=srt_expired_access" });
    expect(res.status).toBe(200);
    expect(bearer).toBe("Bearer sat_renewed");
    const cookies = res.headers.getSetCookie().join("\n");
    expect(cookies).toContain("osp_at=sat_renewed");
    expect(cookies).toContain("osp_rt=srt_renewed");
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
      cookie: "osp_at=sat_stale; osp_rt=srt_for_retry",
      headers: { origin: "http://localhost:3100", "content-type": "application/json" },
    });
    expect(res.status).toBe(201);
    expect(received).toEqual(['{"file_name":"a.bin"}']);
    expect(res.headers.getSetCookie().join("\n")).toContain("osp_at=sat_second");
  });

  it("ends the session when the refresh token is refused", async () => {
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () => HttpResponse.json({ error: "no", code: "refresh_reused" }, { status: 401 })),
    );
    const res = await call(GET, "catalog/objects", { cookie: "osp_rt=srt_revoked" });
    expect(res.status).toBe(401);
    const cookies = res.headers.getSetCookie().join("\n");
    expect(cookies).toContain("osp_at=;");
    expect(cookies).toContain("osp_rt=;");
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
      cookie: "osp_at=sat_dl; osp_rt=srt_dl",
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
      cookie: "osp_at=sat_d; osp_rt=srt_d",
      headers: { origin: "http://localhost:3100" },
    });
    expect(res.status).toBe(204);
  });
});
