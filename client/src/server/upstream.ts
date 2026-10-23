import { NextResponse } from "next/server";
import { defaultMessage } from "@/lib/api/errors";
import { logServerError } from "./log";

// A failed server call (5xx) is logged here with what the API said, and the
// browser gets only a generic message, the status, and the request id.
export async function hideServerFailure(upstream: Response, where: string) {
  const text = await upstream.text().catch(() => "");
  const requestId = upstream.headers.get("x-request-id") ?? undefined;
  logServerError("upstream_failure", { where, status: upstream.status, requestId, body: text });

  let code = "internal";
  try {
    const parsed = JSON.parse(text) as { code?: unknown };
    if (typeof parsed.code === "string" && /^[a-z_]{1,40}$/.test(parsed.code)) code = parsed.code;
  } catch {
    // not JSON; the generic code stands
  }

  const res = NextResponse.json({ error: defaultMessage(upstream.status), code }, { status: upstream.status });
  if (requestId) res.headers.set("x-request-id", requestId);
  const retryAfter = upstream.headers.get("retry-after");
  if (retryAfter) res.headers.set("retry-after", retryAfter);
  return res;
}
