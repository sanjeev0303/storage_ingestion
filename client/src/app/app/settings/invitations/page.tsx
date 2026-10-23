import type { Metadata } from "next";
import { Invitations } from "@/features/organization/components/invitations";

export const metadata: Metadata = { title: "Invitations" };

export default function Page() {
  return <Invitations />;
}
