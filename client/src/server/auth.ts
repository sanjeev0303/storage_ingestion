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

// clientIP is the caller's address (clientAddress), passed on so the API's
// per-address refresh limit counts each caller, not the console as a whole.
export async function refreshTokens(refreshToken: string, clientIP = ""): Promise<Tokens | null> {
  const now = Date.now();
  for (const [token, entry] of recent) {
    if (entry.until < now) recent.delete(token);
  }

  const done = recent.get(refreshToken);
  if (done) return done.tokens;

  const running = inflight.get(refreshToken);
  if (running) return running;

  const attempt = requestRefresh(refreshToken, clientIP)
    .then((tokens) => {
      recent.set(refreshToken, { tokens, until: Date.now() + RECENT_MS });
      return tokens;
    })
    .finally(() => inflight.delete(refreshToken));
  inflight.set(refreshToken, attempt);
  return attempt;
}

async function requestRefresh(refreshToken: string, clientIP: string): Promise<Tokens | null> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (clientIP) headers["x-forwarded-for"] = clientIP;
  const res = await fetch(`${BACKEND_URL}/api/v1/auth/refresh`, {
    method: "POST",
    headers,
    body: JSON.stringify({ refresh_token: refreshToken }),
    cache: "no-store",
  });
  if (res.status === 401 || res.status === 403) return null;
  if (!res.ok) throw new Error(`refresh failed with ${res.status}`);
  return (await res.json()) as Tokens;
}

// How many proxies the console runs behind. Each appends the address it saw
// to X-Forwarded-For, so the caller is that many entries from the end, and
// anything before is whatever the caller wrote itself. With none (local
// development), Next.js sets the header to the connection's address only
// when the request has none: a caller reaching the console directly can
// still choose its own, so production runs the console behind a proxy.
const TRUSTED_PROXY_HOPS = Math.max(0, Number.parseInt(process.env.TRUSTED_PROXY_HOPS ?? "0", 10) || 0);

// An IPv4 or IPv6 address, and nothing else that could ride into a header.
const ADDRESS = /^[0-9A-Fa-f.:]{2,45}$/;

// The caller's address, sent to the API as the one X-Forwarded-For entry it
// trusts (the last). Empty when there is none worth trusting.
export function clientAddress(req: NextRequest) {
  const entries = (req.headers.get("x-forwarded-for") ?? "")
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean);
  const address = entries[entries.length - Math.max(TRUSTED_PROXY_HOPS, 1)] ?? "";
  return ADDRESS.test(address) ? address : "";
}

// Cookies are SameSite=Strict, which already stops cross-site requests. The
// Origin check is a second guard for browsers that are lax about it.
export function isSameOrigin(req: NextRequest) {
  const origin = req.headers.get("origin");
  if (origin && origin !== req.nextUrl.origin) return false;
  const site = req.headers.get("sec-fetch-site");
  return !site || site === "same-origin" || site === "none";
}
