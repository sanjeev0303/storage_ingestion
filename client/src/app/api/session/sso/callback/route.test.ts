// @vitest-environment node
import { http, HttpResponse } from "msw";
import { NextRequest } from "next/server";
import { describe, expect, it } from "vitest";
import { server } from "@/test/server";
import { GET } from "./route";

const BACKEND = "http://localhost:8080/api/v1";

function callback(stateCookie?: string) {
  const headers: Record<string, string> = { "sec-fetch-site": "cross-site" };
  if (stateCookie) headers.cookie = `orb_sso_state=${stateCookie}`;
  return GET(new NextRequest("http://localhost:3100/api/session/sso/callback?code=c1&state=s1", { headers }));
}

function signedIn() {
  return HttpResponse.json({
    access_token: "sat_sso",
    access_expires_at: new Date(Date.now() + 900_000).toISOString(),
    refresh_token: "srt_sso",
    refresh_expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    session_id: "s1",
    organization_id: "org-1",
  });
}

describe("single sign-on callback", () => {
  it("refuses a code and state this browser did not ask for", async () => {
    let called = false;
    server.use(
      http.post(`${BACKEND}/auth/sso/callback`, () => {
        called = true;
        return signedIn();
      }),
    );
    // Someone else's sign-in, opened in this browser through a link.
    const res = await callback();
    expect(called).toBe(false);
    expect(await res.text()).toContain("/login?sso=refused");
    expect(res.headers.get("set-cookie") ?? "").not.toContain("orb_rt=");
  });

  it("finishes the sign-in this browser started and forgets its state", async () => {
    let sent: unknown;
    server.use(
      http.post(`${BACKEND}/auth/sso/callback`, async ({ request }) => {
        sent = await request.json();
        return signedIn();
      }),
    );
    const res = await callback("s1");
    expect(sent).toEqual({ code: "c1", state: "s1" });
    expect(await res.text()).toContain("/sso/done?org=org-1");
    const cookies = res.headers.get("set-cookie") ?? "";
    expect(cookies).toContain("orb_rt=srt_sso");
    expect(cookies).toMatch(/orb_sso_state=;/);
  });
});
