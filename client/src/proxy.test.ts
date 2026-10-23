// @vitest-environment node
import { NextRequest } from "next/server";
import { describe, expect, it } from "vitest";
import { proxy } from "./proxy";

function request(path: string, cookie?: string) {
  return new NextRequest(`http://localhost:3100${path}`, { headers: cookie ? { cookie } : {} });
}

describe("route guard", () => {
  it("sends signed-out visitors to sign in and remembers where they were going", () => {
    const res = proxy(request("/app/datasets?status=FAILED"));
    expect(res.status).toBe(307);
    const location = new URL(res.headers.get("location")!);
    expect(location.pathname).toBe("/login");
    expect(location.searchParams.get("next")).toBe("/app/datasets?status=FAILED");
  });

  it("lets a session through to the console", () => {
    const res = proxy(request("/app/overview", "osp_rt=srt_x"));
    expect(res.headers.get("location")).toBeNull();
  });

  it("sends a signed-in visitor on from the sign-in page", () => {
    const res = proxy(request("/login", "osp_rt=srt_x"));
    expect(new URL(res.headers.get("location")!).pathname).toBe("/app/overview");
  });

  it("leaves the sign-in page alone when signed out, so a stale cookie cannot cause a redirect loop", () => {
    expect(proxy(request("/login")).headers.get("location")).toBeNull();
  });
});
