// @vitest-environment node
import { http, HttpResponse } from "msw";
import { NextRequest } from "next/server";
import { describe, expect, it } from "vitest";
import { server } from "@/test/server";
import { POST } from "./route";

const BACKEND = "http://localhost:8080/api/v1";

function logout(cookie: string) {
  return POST(
    new NextRequest("http://localhost:3100/api/session/logout", {
      method: "POST",
      headers: { cookie, origin: "http://localhost:3100" },
    }),
  );
}

describe("sign-out", () => {
  it("ends the server session even after the access cookie has expired", async () => {
    let endedWith: string | null = null;
    server.use(
      http.post(`${BACKEND}/auth/refresh`, () =>
        HttpResponse.json({
          access_token: "sat_late",
          access_expires_at: new Date(Date.now() + 900_000).toISOString(),
          refresh_token: "srt_late_next",
          refresh_expires_at: new Date(Date.now() + 86_400_000).toISOString(),
          session_id: "s1",
        }),
      ),
      http.post(`${BACKEND}/auth/logout`, ({ request }) => {
        endedWith = request.headers.get("authorization");
        return new HttpResponse(null, { status: 204 });
      }),
    );

    const res = await logout("osp_rt=srt_late");
    expect(res.status).toBe(204);
    expect(endedWith).toBe("Bearer sat_late");
    expect(res.headers.get("set-cookie")).toContain("osp_rt=;");
  });
});
