import type { Metadata } from "next";
import { AdoptionScreen } from "@/features/adoption/components/adoption-screen";

export const metadata: Metadata = { title: "Adopt existing data" };

export default function AdoptPage() {
  return <AdoptionScreen />;
}
