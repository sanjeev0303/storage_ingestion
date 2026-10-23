import type { Metadata } from "next";
import { OrgSettings } from "@/features/organization/components/org-settings";

export const metadata: Metadata = { title: "Organization settings" };

export default function Page() {
  return <OrgSettings />;
}
