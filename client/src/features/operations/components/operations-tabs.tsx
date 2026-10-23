"use client";

import { TabNav } from "@/components/ui/page";
import { useSession } from "@/lib/auth/session";
import { operationsTabs } from "./tabs";

export function OperationsTabs() {
  const session = useSession();
  return <TabNav tabs={operationsTabs(session)} label="Operations" />;
}
