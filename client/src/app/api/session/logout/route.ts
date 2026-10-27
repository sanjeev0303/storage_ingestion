import { NextRequest, NextResponse } from "next/server";
import { ACCESS_COOKIE, BACKEND_URL, REFRESH_COOKIE, clearSessionCookies, clientAddress, isSameOrigin, refreshTokens } from "@/server/auth";

export async function POST(req: NextRequest) {
  if (!isSameOrigin(req)) {
    return NextResponse.json({ error: "cross-origin request refused", code: "forbidden" }, { status: 403 });
  }

  // Ending the session on the server is best effort; the cookies go either way.
  // An expired access cookie is renewed first, or the 30-day refresh session
  // would outlive the sign-out.
  let access = req.cookies.get(ACCESS_COOKIE)?.value;
  const refresh = req.cookies.get(REFRESH_COOKIE)?.value;
  if (!access && refresh) {
    access = (await refreshTokens(refresh, clientAddress(req)).catch(() => null))?.access_token;
  }
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
