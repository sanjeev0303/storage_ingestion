import { NextRequest, NextResponse } from "next/server";
import {
  ACCESS_COOKIE,
  BACKEND_URL,
  RECENT_MS,
  REFRESH_COOKIE,
  RefreshUnavailable,
  clearSessionCookies,
  clientAddress,
  isSameOrigin,
  refreshTokens,
  setSessionCookies,
  type Tokens,
} from "@/server/auth";
import { logServerError } from "@/server/log";
import { hideServerFailure } from "@/server/upstream";

// Everything the browser may send to the API. Cookies and anything else stay behind.
const FORWARD_REQUEST_HEADERS = [
  "content-type",
  "accept",
  "range",
  "if-range",
  "if-match",
  "if-none-match",
  "if-modified-since",
  "if-unmodified-since",
  "x-manifest-sha256",
  "idempotency-key",
  "x-organization-id",
  "x-request-id",
];

const FORWARD_RESPONSE_HEADERS = [
  "content-type",
  "content-length",
  "content-range",
  "content-disposition",
  "accept-ranges",
  "etag",
  "last-modified",
  "location",
  "retry-after",
  "cache-control",
  "x-content-type-options",
  "x-request-id",
];

// Tokens are only ever issued by /api/session/login and /api/session/register.
// Letting the browser call these would hand tokens to script.
const BLOCKED_PATHS = new Set(["auth/login", "auth/register", "auth/refresh"]);

// Routes that work without a session; the backend still checks the token inside.
const PUBLIC_PATHS = new Set(["auth/activate", "auth/password/reset", "invitations/accept"]);
const isPublic = (path: string) => PUBLIC_PATHS.has(path) || path.startsWith("downloads/");

// The largest body the console forwards: an inline manifest (16 MiB) and
// some room. Files themselves go straight to storage, never through here.
const MAX_BODY_BYTES = 17 * 1024 * 1024;

class BodyTooLarge extends Error {}

// Reads the request body, giving up as soon as it passes MAX_BODY_BYTES
// rather than holding however much a caller sends.
async function readBody(req: NextRequest): Promise<ArrayBuffer | undefined> {
  if (!req.body) return undefined;
  const reader = req.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > MAX_BODY_BYTES) {
      await reader.cancel();
      throw new BodyTooLarge();
    }
    chunks.push(value);
  }
  const body = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body.buffer;
}

async function handle(req: NextRequest, ctx: RouteContext<"/api/proxy/[...path]">) {
  const { path: segments } = await ctx.params;
  const path = segments.join("/");
  // Logged in place of the path: a download link's path is its token.
  const where = `proxy ${req.method} ${path.startsWith("downloads/") ? "downloads/:token" : path}`;

  if (BLOCKED_PATHS.has(path)) {
    return NextResponse.json({ error: "not found", code: "not_found" }, { status: 404 });
  }
  const unsafe = !["GET", "HEAD"].includes(req.method);
  if (unsafe && !isSameOrigin(req)) {
    return NextResponse.json({ error: "cross-origin request refused", code: "forbidden" }, { status: 403 });
  }

  const headers = new Headers();
  for (const name of FORWARD_REQUEST_HEADERS) {
    const value = req.headers.get(name);
    if (value) headers.set(name, value);
  }
  const address = clientAddress(req);
  if (address) headers.set("x-forwarded-for", address);

  const arrived = Date.now();
  let access = req.cookies.get(ACCESS_COOKIE)?.value;
  const refresh = req.cookies.get(REFRESH_COOKIE)?.value;
  // No session at all: refused before a byte of the body is read.
  if (!access && !refresh && !isPublic(path)) return unauthenticated(false);

  const length = req.headers.get("content-length");
  if (length !== null && Number(length) > MAX_BODY_BYTES) {
    return NextResponse.json({ error: "request body too large", code: "payload_too_large" }, { status: 413 });
  }

  let renewed: Tokens | null = null;
  let sessionEnded = false;

  const renew = async () => {
    if (!refresh) return false;
    const tokens = await refreshTokens(refresh, address);
    if (!tokens) {
      sessionEnded = true;
      access = undefined;
      return false;
    }
    renewed = tokens;
    access = tokens.access_token;
    return true;
  };

  // Renewed before the body is read: a slow body must not push the refresh
  // past the window in which a concurrent rotation's answer is still cached.
  if (!access) {
    try {
      await renew();
    } catch (error) {
      return refreshFailed(error, where);
    }
    if (!access && !isPublic(path)) return unauthenticated(sessionEnded);
  }

  let body: ArrayBuffer | undefined;
  // A chunked body has no length: read it, within the same cap.
  if (unsafe && length !== "0") {
    try {
      body = await readBody(req);
    } catch (error) {
      if (!(error instanceof BodyTooLarge)) throw error;
      return NextResponse.json({ error: "request body too large", code: "payload_too_large" }, { status: 413 });
    }
  }
  // Each segment encoded again: a decoded "?" or "#" must not become part
  // of the URL's query or fragment.
  const target = `${BACKEND_URL}/api/v1/${segments.map(encodeURIComponent).join("/")}${req.nextUrl.search}`;

  const send = () => {
    const outgoing = new Headers(headers);
    if (access) outgoing.set("authorization", `Bearer ${access}`);
    return fetch(target, { method: req.method, headers: outgoing, body, cache: "no-store", redirect: "manual" });
  };

  let upstream: Response;
  try {
    upstream = await send();
    if (upstream.status === 401 && refresh && !renewed && !sessionEnded && (await sessionRefused(upstream))) {
      await upstream.body?.cancel();
      // The refresh token came with the request. After RECENT_MS another
      // request may have spent it and its cached answer is gone; presenting
      // it then would revoke the session. Ask for a retry instead, which
      // carries the browser's current cookies.
      if (Date.now() - arrived > RECENT_MS) return renewalUnavailable();
      // The 401's body is gone now; when the session cannot be renewed
      // (revoked, expired), answer with our own 401.
      if (!(await renew())) return unauthenticated(true);
      upstream = await send();
    }
    // Only the API's word that the token or session is no longer valid ends
    // the session; a refused password or SSO check is passed on as it is.
    if (upstream.status === 401 && !isPublic(path) && (await sessionRefused(upstream))) sessionEnded = true;
  } catch (error) {
    if (error instanceof RefreshUnavailable) return refreshFailed(error, where);
    logServerError("upstream_unreachable", { where, error: String(error) });
    return NextResponse.json(
      { error: "the storage API could not be reached", code: "upstream_unreachable" },
      { status: 502 },
    );
  }

  if (upstream.status >= 500) return hideServerFailure(upstream, where);

  const responseHeaders = new Headers();
  for (const name of FORWARD_RESPONSE_HEADERS) {
    const value = upstream.headers.get(name);
    if (value) responseHeaders.set(name, value);
  }
  if (upstream.headers.get("content-encoding")) responseHeaders.delete("content-length");

  const noBody = req.method === "HEAD" || [204, 205, 304].includes(upstream.status);
  const res = new NextResponse(noBody ? null : upstream.body, {
    status: upstream.status,
    headers: responseHeaders,
  });
  // A session the API refused even with renewed tokens is over: clear, never set.
  if (sessionEnded) clearSessionCookies(res);
  else if (renewed) setSessionCookies(res, renewed);
  return res;
}

// The API's 401 codes for a token or session it no longer accepts. The bare
// "authentication required" 401s some handlers send mean the same.
const SESSION_REFUSED_CODES = new Set(["unauthenticated", "refresh_reused"]);

async function sessionRefused(res: Response) {
  let code: unknown;
  try {
    code = (await res.clone().json()).code;
  } catch {
    // not JSON, so no code
  }
  return typeof code !== "string" || SESSION_REFUSED_CODES.has(code);
}

// A refresh that got no answer is not presented again for a short while (see
// RefreshUnavailable), so the browser keeps its cookies and is asked to retry.
function refreshFailed(error: unknown, where: string) {
  logServerError("refresh_unavailable", { where, error: String(error) });
  return renewalUnavailable();
}

function renewalUnavailable() {
  return NextResponse.json(
    { error: "the session could not be renewed just now; try again", code: "refresh_unavailable" },
    { status: 503, headers: { "retry-after": String(Math.ceil(RECENT_MS / 1000)) } },
  );
}

function unauthenticated(clear: boolean) {
  const res = NextResponse.json(
    { error: "authentication required", code: "unauthenticated" },
    { status: 401 },
  );
  if (clear) clearSessionCookies(res);
  return res;
}

export const GET = handle;
export const HEAD = handle;
export const POST = handle;
export const PUT = handle;
export const PATCH = handle;
export const DELETE = handle;
