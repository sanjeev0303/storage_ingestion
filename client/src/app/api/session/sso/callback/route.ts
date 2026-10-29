import type { NextRequest } from "next/server";
import { finishSignIn, refused } from "@/server/sso";

// The OpenID provider sends the browser here with a code and a state; the
// API checks both and answers with a session.
export async function GET(req: NextRequest) {
  const params = req.nextUrl.searchParams;
  if (params.get("error")) {
    return refused(`The identity provider did not sign you in (${params.get("error")}).`);
  }
  const code = params.get("code") ?? "";
  const state = params.get("state") ?? "";
  if (!code || !state) return refused("The identity provider's answer was incomplete. Start again.");
  return finishSignIn(req, "/api/v1/auth/sso/callback", { code, state });
}
