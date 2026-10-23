import { NextRequest, NextResponse } from "next/server";
import { BACKEND_URL, clientAddress, isSameOrigin, setSessionCookies, type Tokens } from "@/server/auth";
import { logServerError } from "@/server/log";
import { hideServerFailure } from "@/server/upstream";

type SessionResponse = Tokens & { token_type: string; user: unknown };

// Sends credentials to a backend endpoint that answers with tokens (sign-in or
// sign-up). The tokens go into HttpOnly cookies; the browser only gets the user.
export async function startSession(req: NextRequest, backendPath: string) {
  if (!isSameOrigin(req)) {
    return NextResponse.json({ error: "cross-origin request refused", code: "forbidden" }, { status: 403 });
  }

  const headers = new Headers({ "Content-Type": "application/json" });
  const address = clientAddress(req);
  if (address) headers.set("x-forwarded-for", address);

  let upstream: Response;
  try {
    upstream = await fetch(`${BACKEND_URL}${backendPath}`, {
      method: "POST",
      headers,
      body: await req.text(),
      cache: "no-store",
    });
  } catch (error) {
    logServerError("upstream_unreachable", { where: `session ${backendPath}`, error: String(error) });
    return NextResponse.json(
      { error: "the storage API could not be reached", code: "upstream_unreachable" },
      { status: 502 },
    );
  }

  if (upstream.status >= 500) return hideServerFailure(upstream, `session ${backendPath}`);
  const text = await upstream.text();
  if (!upstream.ok) {
    const failed = new NextResponse(text, {
      status: upstream.status,
      headers: { "content-type": upstream.headers.get("content-type") ?? "text/plain" },
    });
    const retryAfter = upstream.headers.get("retry-after");
    if (retryAfter) failed.headers.set("retry-after", retryAfter);
    return failed;
  }

  const session = JSON.parse(text) as SessionResponse;
  const res = NextResponse.json({ user: session.user, session_id: session.session_id });
  res.headers.set("cache-control", "no-store");
  setSessionCookies(res, session);
  return res;
}
