import type { Metadata } from "next";
import { StatusScreen } from "@/features/operations/components/status-screen";

export const metadata: Metadata = { title: "Operations status" };

export default function StatusPage() {
  return <StatusScreen />;
}
