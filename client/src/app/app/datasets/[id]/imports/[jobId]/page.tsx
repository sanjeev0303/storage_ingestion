import type { Metadata } from "next";
import { ImportJob } from "@/features/datasets/components/import-job";

export const metadata: Metadata = { title: "Manifest import" };

export default async function ImportJobPage({ params }: PageProps<"/app/datasets/[id]/imports/[jobId]">) {
  const { id, jobId } = await params;
  return <ImportJob id={id} jobId={jobId} />;
}
