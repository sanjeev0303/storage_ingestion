import type { Metadata } from "next";
import { VersionDetail } from "@/features/datasets/components/version-detail";

export const metadata: Metadata = { title: "Dataset version" };

export default async function VersionPage({ params }: PageProps<"/app/datasets/[id]/versions/[versionId]">) {
  const { id, versionId } = await params;
  return <VersionDetail id={id} versionId={versionId} />;
}
