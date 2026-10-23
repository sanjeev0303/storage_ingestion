// @vitest-environment node
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { server } from "@/test/server";
import { BACKEND_URL, refreshTokens, type Tokens } from "./auth";

const refreshUrl = `${BACKEND_URL}/api/v1/auth/refresh`;

function tokens(label: string): Tokens {
  return {
    access_token: `sat_${label}`,
    access_expires_at: new Date(Date.now() + 900_000).toISOString(),
    refresh_token: `srt_${label}`,
    refresh_expires_at: new Date(Date.now() + 86_400_000).toISOString(),
    session_id: "session-1",
  };
}

describe("refreshTokens", () => {
  it("shares one refresh between concurrent callers (a spent refresh token revokes the session)", async () => {
    let calls = 0;
    server.use(
      http.post(refreshUrl, async () => {
        calls++;
        await new Promise((r) => setTimeout(r, 20));
        return HttpResponse.json(tokens("a"));
      }),
    );
    const [one, two, three] = await Promise.all([
      refreshTokens("srt_old_a"),
      refreshTokens("srt_old_a"),
      refreshTokens("srt_old_a"),
    ]);
    expect(calls).toBe(1);
    expect(one?.access_token).toBe("sat_a");
    expect(two).toBe(one);
    expect(three).toBe(one);
  });

  it("answers a late request that still carries the old cookie without refreshing again", async () => {
    let calls = 0;
    server.use(
      http.post(refreshUrl, () => {
        calls++;
        return HttpResponse.json(tokens("b"));
      }),
    );
    await refreshTokens("srt_old_b");
    const late = await refreshTokens("srt_old_b");
    expect(calls).toBe(1);
    expect(late?.access_token).toBe("sat_b");
  });

  it("returns null when the refresh token was refused", async () => {
    server.use(
      http.post(refreshUrl, () => HttpResponse.json({ error: "x", code: "refresh_reused" }, { status: 401 })),
    );
    expect(await refreshTokens("srt_old_c")).toBeNull();
  });

  it("throws on a server failure so the session is not dropped by a blip", async () => {
    server.use(http.post(refreshUrl, () => new HttpResponse(null, { status: 500 })));
    await expect(refreshTokens("srt_old_d")).rejects.toThrow();
  });
});
