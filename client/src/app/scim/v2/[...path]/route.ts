import { NextResponse, type NextRequest } from "next/server";
import { BACKEND_URL, clientAddress } from "@/server/auth";
import { logServerError } from "@/server/log";

// SCIM for identity providers: <console>/scim/v2/... goes to the API as it
// is, with the provider's own bearer token. No cookies are read or sent.
async function forward(req: NextRequest, ctx: { params: Promise<{ path: string[] }> }) {
  const { path } = await ctx.params;
  const target = `${BACKEND_URL}/scim/v2/${path.map(encodeURIComponent).join("/")}${req.nextUrl.search}`;
  const headers = new Headers();
  for (const name of ["authorization", "content-type", "accept"]) {
    const value = req.headers.get(name);
    if (value) headers.set(name, value);
  }
  const address = clientAddress(req);
  if (address) headers.set("x-forwarded-for", address);
  const body = req.method === "GET" || req.method === "DELETE" ? undefined : await req.text();
  if (body !== undefined && body.length > 1024 * 1024) {
    return NextResponse.json({ status: "413", detail: "request body too large" }, { status: 413 });
  }
  let upstream: Response;
  try {
    upstream = await fetch(target, { method: req.method, headers, body, cache: "no-store", redirect: "manual" });
  } catch (error) {
    logServerError("upstream_unreachable", { where: "scim", error: String(error) });
    return NextResponse.json({ status: "502", detail: "the storage API could not be reached" }, { status: 502 });
  }
  const out = new NextResponse(upstream.status === 204 ? null : upstream.body, { status: upstream.status });
  for (const name of ["content-type", "www-authenticate", "retry-after"]) {
    const value = upstream.headers.get(name);
    if (value) out.headers.set(name, value);
  }
  out.headers.set("cache-control", "no-store");
  return out;
}

export { forward as GET, forward as POST, forward as PUT, forward as PATCH, forward as DELETE };
