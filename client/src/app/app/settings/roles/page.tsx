import type { Metadata } from "next";
import { Roles } from "@/features/organization/components/roles";

export const metadata: Metadata = { title: "Roles" };

export default function Page() {
  return <Roles />;
}
