// @vitest-environment node
import { http, HttpResponse } from "msw";
import { NextRequest } from "next/server";
import { describe, expect, it } from "vitest";
import { server } from "@/test/server";
import { POST } from "./route";

const BACKEND = "http://localhost:8080/api/v1";

function post(fields: Record<string, string>) {
  return POST(
    new NextRequest("http://localhost:3100/api/session/saml/acs", {
      method: "POST",
      // The provider's page posts the form from its own site.
      headers: { "content-type": "application/x-www-form-urlencoded", origin: "https://idp.example.com" },
      body: new URLSearchParams(fields).toString(),
    }),
  );
}

describe("SAML assertion consumer service", () => {
  it("passes the response to the API and starts the session", async () => {
    let sent: unknown;
    server.use(
      http.post(`${BACKEND}/auth/saml/acs`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({
          access_token: "sat_saml",
          access_expires_at: new Date(Date.now() + 900_000).toISOString(),
          refresh_token: "srt_saml",
          refresh_expires_at: new Date(Date.now() + 86_400_000).toISOString(),
          session_id: "s1",
          organization_id: "org-1",
        });
      }),
    );

    const res = await post({ SAMLResponse: "PHNhbWxwOlJlc3BvbnNlLz4=", RelayState: "state-1" });
    expect(sent).toEqual({ saml_response: "PHNhbWxwOlJlc3BvbnNlLz4=", relay_state: "state-1" });
    expect(res.status).toBe(200);
    expect(await res.text()).toContain("/sso/done?org=org-1");
    expect(res.headers.get("set-cookie")).toContain("osp_rt=srt_saml");
  });

  it("sends a refusal to the sign-in page without starting a session", async () => {
    server.use(
      http.post(`${BACKEND}/auth/saml/acs`, () =>
        HttpResponse.json({ error: "this sign-in was refused" }, { status: 401 }),
      ),
    );
    const res = await post({ SAMLResponse: "eA==", RelayState: "state-1" });
    expect(await res.text()).toContain("/login?sso=refused");
    const cookies = res.headers.get("set-cookie") ?? "";
    expect(cookies).toContain("osp_sso_error=");
    expect(cookies).not.toContain("osp_rt=");
  });

  it("refuses an incomplete post before calling the API", async () => {
    const res = await post({ SAMLResponse: "eA==" });
    expect(await res.text()).toContain("/login?sso=refused");
  });
});
