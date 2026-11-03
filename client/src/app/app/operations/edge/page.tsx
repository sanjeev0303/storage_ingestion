import type { Metadata } from "next";
import { EdgeScreen } from "@/features/edge/components/edge-screen";

export const metadata: Metadata = { title: "Edge devices" };

export default function EdgePage() {
  return <EdgeScreen />;
}
