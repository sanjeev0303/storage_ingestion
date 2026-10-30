import type { Metadata } from "next";
import { AccessScreen } from "@/features/access/components/access-screen";

export const metadata: Metadata = { title: "Access" };

export default function AccessPage() {
  return <AccessScreen />;
}
