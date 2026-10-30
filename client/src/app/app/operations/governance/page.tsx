import type { Metadata } from "next";
import { GovernanceScreen } from "@/features/governance/components/governance-screen";

export const metadata: Metadata = { title: "Governance" };

export default function GovernancePage() {
  return <GovernanceScreen />;
}
