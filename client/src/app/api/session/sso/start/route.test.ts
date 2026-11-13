// @vitest-environment node
import { http, HttpResponse } from "msw";
import { NextRequest } from "next/server";
import { describe, expect, it } from "vitest";
import { server } from "@/test/server";
import { POST } from "./route";

const BACKEND = "http://localhost:8080/api/v1";

function start() {
  return POST(
    new NextRequest("http://localhost:3100/api/session/sso/start", {
      method: "POST",
      headers: { "content-type": "application/json", origin: "http://localhost:3100", "sec-fetch-site": "same-origin" },
      body: JSON.stringify({ organization: "acme" }),
    }),
  );
}

describe("single sign-on start", () => {
  it("binds the sign-in to this browser with its state", async () => {
    server.use(
      http.post(`${BACKEND}/auth/sso/start`, () =>
        HttpResponse.json({ authorization_url: "https://idp.example.com/authorize?client_id=x&state=s1" }),
      ),
    );
    const res = await start();
    expect(res.status).toBe(200);
    const cookie = res.headers.get("set-cookie") ?? "";
    expect(cookie).toContain("orb_sso_state=s1");
    expect(cookie).toContain("HttpOnly");
    expect(cookie).toContain("Path=/api/session");
  });

  it("binds a SAML sign-in by its RelayState", async () => {
    server.use(
      http.post(`${BACKEND}/auth/sso/start`, () =>
        HttpResponse.json({ authorization_url: "https://idp.example.com/sso?SAMLRequest=abc&RelayState=r1" }),
      ),
    );
    const res = await start();
    expect(res.headers.get("set-cookie") ?? "").toContain("orb_sso_state=r1");
  });
});
