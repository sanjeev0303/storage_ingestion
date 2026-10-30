import type { Metadata } from "next";
import { ErasureScreen } from "@/features/erasure/components/erasure-screen";

export const metadata: Metadata = { title: "Erasure" };

export default function ErasurePage() {
  return <ErasureScreen />;
}
