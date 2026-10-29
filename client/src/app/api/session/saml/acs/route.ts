import type { NextRequest } from "next/server";
import { finishSignIn, refused } from "@/server/sso";

// The SAML assertion consumer service: the identity provider's page posts
// its signed response here (HTTP-POST binding). A cross-site form post by
// design, so there is no same-origin check: the API checks the signature,
// the audience and that the response answers a sign-in it started.
export async function POST(req: NextRequest) {
  let form: FormData;
  try {
    form = await req.formData();
  } catch {
    return refused("The identity provider's answer could not be read. Start again.");
  }
  const samlResponse = form.get("SAMLResponse");
  const relayState = form.get("RelayState");
  if (typeof samlResponse !== "string" || typeof relayState !== "string" || !samlResponse || !relayState) {
    return refused("The identity provider's answer was incomplete. Start again.");
  }
  return finishSignIn(req, "/api/v1/auth/saml/acs", { saml_response: samlResponse, relay_state: relayState });
}
