import type { Metadata } from "next";
import { Members } from "@/features/organization/components/members";

export const metadata: Metadata = { title: "Members" };

export default function Page() {
  return <Members />;
}
