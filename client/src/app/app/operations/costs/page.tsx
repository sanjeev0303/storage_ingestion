import type { Metadata } from "next";
import { CostsScreen } from "@/features/tiering/components/costs-screen";

export const metadata: Metadata = { title: "Costs and tiers" };

export default function CostsPage() {
  return <CostsScreen />;
}
