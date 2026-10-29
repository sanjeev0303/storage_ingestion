"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { useEffect } from "react";
import { storeOrganization } from "@/lib/auth/session";

// Opens the console in the organization the person just signed in to.
export function SsoDone() {
  const router = useRouter();
  const params = useSearchParams();

  useEffect(() => {
    const org = params.get("org");
    if (org && /^[A-Za-z0-9_-]{1,64}$/.test(org)) storeOrganization(org);
    router.replace("/app/overview");
  }, [params, router]);

  return <p className="text-base text-muted">Signing in…</p>;
}
