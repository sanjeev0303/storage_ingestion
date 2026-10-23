import { NextResponse, type NextRequest } from "next/server";
import { REFRESH_COOKIE } from "@/server/auth";

// A cheap gate: no session cookie means no console. The cookie only proves a
// session was started; the API decides whether it is still valid, and a 401
// from it clears the cookies and sends the user to sign in.
export function proxy(req: NextRequest) {
  const { pathname } = req.nextUrl;
  const signedIn = req.cookies.has(REFRESH_COOKIE);

  if (pathname.startsWith("/app") && !signedIn) {
    const url = req.nextUrl.clone();
    url.pathname = "/login";
    url.search = `?next=${encodeURIComponent(pathname + req.nextUrl.search)}`;
    return NextResponse.redirect(url);
  }

  if ((pathname === "/login" || pathname === "/register") && signedIn) {
    const url = req.nextUrl.clone();
    url.pathname = "/app/overview";
    url.search = "";
    return NextResponse.redirect(url);
  }

  return NextResponse.next();
}

export const config = {
  matcher: ["/app/:path*", "/login", "/register"],
};
