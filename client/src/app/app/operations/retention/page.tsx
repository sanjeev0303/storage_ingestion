import type { Metadata } from "next";
import { RetentionScreen } from "@/features/operations/components/retention-screen";

export const metadata: Metadata = { title: "Retention and legal holds" };

export default function RetentionPage() {
  return <RetentionScreen />;
}
