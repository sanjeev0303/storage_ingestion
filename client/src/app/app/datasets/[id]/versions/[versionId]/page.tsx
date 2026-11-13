import type { Metadata } from "next";
import { VersionDetail } from "@/features/datasets/components/version-detail";

export const metadata: Metadata = { title: "Dataset version" };

export default async function VersionPage({ params }: PageProps<"/app/datasets/[id]/versions/[versionId]">) {
  const { id, versionId } = await params;
  // keyed so moving to another version, which Next does without remounting, starts with fresh panels
  return <VersionDetail key={versionId} id={id} versionId={versionId} />;
}
