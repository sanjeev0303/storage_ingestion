import type { Metadata } from "next";
import { QuotaTab } from "@/features/organization/components/quota";

export const metadata: Metadata = { title: "Quota" };

export default function Page() {
  return <QuotaTab />;
}
