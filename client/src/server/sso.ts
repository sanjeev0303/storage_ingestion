import "server-only";
import { NextResponse, type NextRequest } from "next/server";
import { BACKEND_URL, clientAddress, setSessionCookies, type Tokens } from "@/server/auth";
import { logServerError } from "@/server/log";

// The provider sends the browser back here. The API only accepts callbacks
// listed in its AUTH_SSO_REDIRECT_URIS, so this must be one of them.
export function ssoCallbackUrl(req: NextRequest) {
  return `${process.env.CONSOLE_ORIGIN ?? req.nextUrl.origin}/api/session/sso/callback`;
}

// A refused sign-in's reason travels to the sign-in page in a short-lived
// cookie, never in the URL: it can name the person's email address.
export const SSO_ERROR_COOKIE = "osp_sso_error";

// A page that moves on by itself rather than a redirect: the session
// cookies are SameSite=Strict, and a redirect chain that began at the
// provider's site would not carry them to the next page.
function moveOn(to: string) {
  return new NextResponse(
    `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=${to}"><title>Signing in</title><p>Signing in. <a href="${to}">Continue</a></p>`,
    { headers: { "content-type": "text/html; charset=utf-8", "cache-control": "no-store", "referrer-policy": "no-referrer" } },
  );
}

export function refused(reason: string) {
  const res = moveOn("/login?sso=refused");
  res.cookies.set(SSO_ERROR_COOKIE, reason.slice(0, 300), { httpOnly: true, sameSite: "lax", path: "/login", maxAge: 60 });
  return res;
}

// Sends the provider's answer to the API, and on success starts the
// session in the organization signed in to.
export async function finishSignIn(req: NextRequest, backendPath: string, body: Record<string, string>) {
  const headers = new Headers({ "Content-Type": "application/json" });
  const address = clientAddress(req);
  if (address) headers.set("x-forwarded-for", address);
  let upstream: Response;
  try {
    upstream = await fetch(`${BACKEND_URL}${backendPath}`, { method: "POST", headers, body: JSON.stringify(body), cache: "no-store" });
  } catch (error) {
    logServerError("upstream_unreachable", { where: backendPath, error: String(error) });
    return refused("The storage API could not be reached. Try again.");
  }
  const text = await upstream.text();
  if (!upstream.ok) {
    if (upstream.status >= 500) logServerError("upstream_failure", { where: backendPath, status: upstream.status, body: text });
    let reason = "Single sign-on failed. Start again.";
    try {
      const answer = JSON.parse(text) as { error?: string };
      if (upstream.status < 500 && answer.error) reason = answer.error;
    } catch {
      // keep the generic reason
    }
    return refused(reason);
  }
  const session = JSON.parse(text) as Tokens & { organization_id: string };
  // The organization id is not secret; the next page makes it the active one.
  const res = moveOn(`/sso/done?org=${encodeURIComponent(session.organization_id)}`);
  setSessionCookies(res, session);
  return res;
}
