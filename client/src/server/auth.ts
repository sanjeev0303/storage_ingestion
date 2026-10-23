import "server-only";
import type { NextRequest, NextResponse } from "next/server";

export const BACKEND_URL = (process.env.BACKEND_URL ?? "http://localhost:8080").replace(/\/$/, "");

export const ACCESS_COOKIE = "osp_at";
export const REFRESH_COOKIE = "osp_rt";

export type Tokens = {
  access_token: string;
  access_expires_at: string;
  refresh_token: string;
  refresh_expires_at: string;
  session_id: string;
};

const secure = process.env.COOKIE_SECURE
  ? process.env.COOKIE_SECURE === "true"
  : process.env.NODE_ENV === "production";

function maxAgeSeconds(expiresAt: string, marginSeconds = 0) {
  const seconds = Math.floor((Date.parse(expiresAt) - Date.now()) / 1000) - marginSeconds;
  return Math.max(seconds, 0);
}

export function setSessionCookies(res: NextResponse, tokens: Tokens) {
  const base = { httpOnly: true, sameSite: "strict" as const, secure, path: "/" };
  // The access cookie expires a little early so a request never carries a token
  // that dies in flight; the proxy then refreshes before sending.
  res.cookies.set(ACCESS_COOKIE, tokens.access_token, {
    ...base,
    maxAge: maxAgeSeconds(tokens.access_expires_at, 20),
  });
  res.cookies.set(REFRESH_COOKIE, tokens.refresh_token, {
    ...base,
    maxAge: maxAgeSeconds(tokens.refresh_expires_at),
  });
}

export function clearSessionCookies(res: NextResponse) {
  res.cookies.delete(ACCESS_COOKIE);
  res.cookies.delete(REFRESH_COOKIE);
}

// Refresh tokens are single use: presenting a spent one revokes the whole
// session. Concurrent requests that carry the same old token must share one
// refresh, and requests that were already in flight with the old cookie get
// the same answer for a short while afterwards.
const inflight = new Map<string, Promise<Tokens | null>>();
const recent = new Map<string, { tokens: Tokens | null; until: number }>();
const RECENT_MS = 30_000;

export async function refreshTokens(refreshToken: string): Promise<Tokens | null> {
  const now = Date.now();
  for (const [token, entry] of recent) {
    if (entry.until < now) recent.delete(token);
  }

  const done = recent.get(refreshToken);
  if (done) return done.tokens;

  const running = inflight.get(refreshToken);
  if (running) return running;

  const attempt = requestRefresh(refreshToken)
    .then((tokens) => {
      recent.set(refreshToken, { tokens, until: Date.now() + RECENT_MS });
      return tokens;
    })
    .finally(() => inflight.delete(refreshToken));
  inflight.set(refreshToken, attempt);
  return attempt;
}

async function requestRefresh(refreshToken: string): Promise<Tokens | null> {
  const res = await fetch(`${BACKEND_URL}/api/v1/auth/refresh`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ refresh_token: refreshToken }),
    cache: "no-store",
  });
  if (res.status === 401 || res.status === 403) return null;
  if (!res.ok) throw new Error(`refresh failed with ${res.status}`);
  return (await res.json()) as Tokens;
}

export function clientAddress(req: NextRequest) {
  const forwarded = req.headers.get("x-forwarded-for");
  const direct = req.headers.get("x-real-ip");
  return forwarded ?? direct ?? "";
}

// Cookies are SameSite=Strict, which already stops cross-site requests. The
// Origin check is a second guard for browsers that are lax about it.
export function isSameOrigin(req: NextRequest) {
  const origin = req.headers.get("origin");
  if (origin && origin !== req.nextUrl.origin) return false;
  const site = req.headers.get("sec-fetch-site");
  return !site || site === "same-origin" || site === "none";
}
