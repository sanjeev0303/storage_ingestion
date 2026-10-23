import type { NextRequest } from "next/server";
import { startSession } from "@/server/sign-in";

export function POST(req: NextRequest) {
  return startSession(req, "/api/v1/auth/login");
}
