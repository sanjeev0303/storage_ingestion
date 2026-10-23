import type { Metadata } from "next";
import { VersionsScreen } from "@/features/datasets/components/versions-screen";

export const metadata: Metadata = { title: "Dataset versions" };

export default async function VersionsPage({ params }: PageProps<"/app/datasets/[id]/versions">) {
  const { id } = await params;
  return <VersionsScreen id={id} />;
}
