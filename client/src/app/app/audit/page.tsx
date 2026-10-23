import type { Metadata } from "next";
import { AuditScreen } from "@/features/audit/components/audit-screen";

export const metadata: Metadata = { title: "Audit log" };

export default function AuditPage() {
  return <AuditScreen />;
}
