import type { Metadata } from "next";
import { BranchesScreen } from "@/features/datasets/components/branches-screen";

export const metadata: Metadata = { title: "Dataset branches" };

export default async function BranchesPage({ params }: PageProps<"/app/datasets/[id]/branches">) {
  const { id } = await params;
  return <BranchesScreen id={id} />;
}
