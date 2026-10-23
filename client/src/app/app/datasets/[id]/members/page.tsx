import type { Metadata } from "next";
import { MembersScreen } from "@/features/datasets/components/members-screen";

export const metadata: Metadata = { title: "Dataset members" };

export default async function MembersPage({ params }: PageProps<"/app/datasets/[id]/members">) {
  const { id } = await params;
  return <MembersScreen id={id} />;
}
