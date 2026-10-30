import type { Metadata } from "next";
import { ContentPolicyScreen } from "@/features/content-checks/components/content-policy-screen";

export const metadata: Metadata = { title: "Content checks" };

export default function Page() {
  return <ContentPolicyScreen />;
}
