import { NextResponse, type NextRequest } from "next/server";
import { BACKEND_URL, clientAddress, isSameOrigin } from "@/server/auth";
import { logServerError } from "@/server/log";
import { ssoCallbackUrl } from "@/server/sso";
import { hideServerFailure } from "@/server/upstream";

// Starts a single sign-on: the API answers with the identity provider's
// address, and the browser goes there.
export async function POST(req: NextRequest) {
  if (!isSameOrigin(req)) {
    return NextResponse.json({ error: "cross-origin request refused", code: "forbidden" }, { status: 403 });
  }
  let organization = "";
  try {
    const body = (await req.json()) as { organization?: unknown };
    if (typeof body.organization === "string") organization = body.organization.trim();
  } catch {
    // an unreadable body is answered like an empty one
  }
  if (!organization) {
    return NextResponse.json({ error: "Enter your organization's short name.", code: "invalid_request" }, { status: 400 });
  }

  const headers = new Headers({ "Content-Type": "application/json" });
  const address = clientAddress(req);
  if (address) headers.set("x-forwarded-for", address);
  let upstream: Response;
  try {
    upstream = await fetch(`${BACKEND_URL}/api/v1/auth/sso/start`, {
      method: "POST",
      headers,
      body: JSON.stringify({ organization, redirect_uri: ssoCallbackUrl(req) }),
      cache: "no-store",
    });
  } catch (error) {
    logServerError("upstream_unreachable", { where: "sso start", error: String(error) });
    return NextResponse.json({ error: "the storage API could not be reached", code: "upstream_unreachable" }, { status: 502 });
  }
  if (upstream.status >= 500) return hideServerFailure(upstream, "sso start");
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "content-type": upstream.headers.get("content-type") ?? "application/json", "cache-control": "no-store" },
  });
}
