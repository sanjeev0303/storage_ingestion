import type { Metadata } from "next";
import { ComplianceScreen } from "@/features/evidence/components/compliance-screen";

export const metadata: Metadata = { title: "Compliance" };

export default function AuditCompliancePage() {
  return <ComplianceScreen />;
}
