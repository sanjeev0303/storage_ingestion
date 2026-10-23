import { NextRequest, NextResponse } from "next/server";
import {
  ACCESS_COOKIE,
  BACKEND_URL,
  REFRESH_COOKIE,
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

async function handle(req: NextRequest, ctx: RouteContext<"/api/proxy/[...path]">) {
  const { path: segments } = await ctx.params;
  const path = segments.join("/");

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

  const hasBody = unsafe && req.headers.get("content-length") !== "0";
  const body = hasBody ? await req.arrayBuffer() : undefined;
  const target = `${BACKEND_URL}/api/v1/${path}${req.nextUrl.search}`;

  let access = req.cookies.get(ACCESS_COOKIE)?.value;
  const refresh = req.cookies.get(REFRESH_COOKIE)?.value;
  let renewed: Tokens | null = null;
  let sessionEnded = false;

  const renew = async () => {
    if (!refresh) return false;
    const tokens = await refreshTokens(refresh);
    if (!tokens) {
      sessionEnded = true;
      access = undefined;
      return false;
    }
    renewed = tokens;
    access = tokens.access_token;
    return true;
  };

  const send = () => {
    const outgoing = new Headers(headers);
    if (access) outgoing.set("authorization", `Bearer ${access}`);
    return fetch(target, { method: req.method, headers: outgoing, body, cache: "no-store", redirect: "manual" });
  };

  let upstream: Response;
  try {
    if (!access) await renew();
    if (!access && !isPublic(path)) {
      return unauthenticated(sessionEnded);
    }
    upstream = await send();
    if (upstream.status === 401 && refresh && !renewed && !sessionEnded) {
      await upstream.body?.cancel();
      if (await renew()) upstream = await send();
    }
  } catch (error) {
    logServerError("upstream_unreachable", { where: `proxy ${req.method} ${path}`, error: String(error) });
    return NextResponse.json(
      { error: "the storage API could not be reached", code: "upstream_unreachable" },
      { status: 502 },
    );
  }

  if (upstream.status === 401 && !isPublic(path)) sessionEnded = true;
  if (upstream.status >= 500) return hideServerFailure(upstream, `proxy ${req.method} ${path}`);

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
  if (renewed) setSessionCookies(res, renewed);
  if (sessionEnded) clearSessionCookies(res);
  return res;
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
