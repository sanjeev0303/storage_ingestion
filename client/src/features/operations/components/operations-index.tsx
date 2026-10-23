"use client";

import { useRouter } from "next/navigation";
import { useEffect } from "react";
import { SkeletonRows } from "@/components/ui/states";
import { useSession } from "@/lib/auth/session";
import { operationsTabs } from "./tabs";

export function OperationsIndex() {
  const session = useSession();
  const router = useRouter();
  const first = operationsTabs(session)[0];

  useEffect(() => {
    router.replace(first.href);
  }, [router, first.href]);

  return <SkeletonRows rows={3} />;
}
