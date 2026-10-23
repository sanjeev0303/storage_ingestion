import { NextRequest, NextResponse } from "next/server";
import { ACCESS_COOKIE, BACKEND_URL, clearSessionCookies, isSameOrigin } from "@/server/auth";

export async function POST(req: NextRequest) {
  if (!isSameOrigin(req)) {
    return NextResponse.json({ error: "cross-origin request refused", code: "forbidden" }, { status: 403 });
  }

  // Ending the session on the server is best effort; the cookies go either way.
  const access = req.cookies.get(ACCESS_COOKIE)?.value;
  if (access) {
    await fetch(`${BACKEND_URL}/api/v1/auth/logout`, {
      method: "POST",
      headers: { Authorization: `Bearer ${access}` },
      cache: "no-store",
    }).catch(() => undefined);
  }

  const res = new NextResponse(null, { status: 204 });
  clearSessionCookies(res);
  return res;
}
