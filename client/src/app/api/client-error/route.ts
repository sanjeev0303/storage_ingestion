import { NextRequest, NextResponse } from "next/server";
import { clientAddress, isSameOrigin } from "@/server/auth";
import { logServerError } from "@/server/log";

// The browser reports a crash here so the real details end up in the server log,
// next to the reference the person was shown, and nowhere on their device.

const MAX_BODY = 8 * 1024;
const WINDOW_MS = 60_000;
const MAX_PER_WINDOW = 30;
const seen = new Map<string, { count: number; start: number }>();

function tooMany(address: string) {
  const now = Date.now();
  const entry = seen.get(address);
  if (!entry || now - entry.start > WINDOW_MS) {
    seen.set(address, { count: 1, start: now });
    return false;
  }
  entry.count += 1;
  return entry.count > MAX_PER_WINDOW;
}

const text = (value: unknown, max: number) => (typeof value === "string" ? value.slice(0, max) : undefined);

export async function POST(req: NextRequest) {
  if (!isSameOrigin(req)) return new NextResponse(null, { status: 403 });
  if (tooMany(clientAddress(req) ?? "unknown")) return new NextResponse(null, { status: 429 });

  const raw = await req.text();
  if (raw.length > MAX_BODY) return new NextResponse(null, { status: 413 });

  let report: Record<string, unknown>;
  try {
    report = JSON.parse(raw) as Record<string, unknown>;
  } catch {
    return new NextResponse(null, { status: 400 });
  }

  logServerError("client_error", {
    reference: text(report.reference, 32),
    digest: text(report.digest, 64),
    source: text(report.source, 32),
    path: text(report.path, 200),
    name: text(report.name, 100),
    message: text(report.message, 1000),
    stack: text(report.stack, 4000),
  });
  return new NextResponse(null, { status: 204 });
}
