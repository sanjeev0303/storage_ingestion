import type { Metadata } from "next";
import { EvidenceScreen } from "@/features/evidence/components/evidence-screen";

export const metadata: Metadata = { title: "Anchors and exports" };

export default function AuditEvidencePage() {
  return <EvidenceScreen />;
}
