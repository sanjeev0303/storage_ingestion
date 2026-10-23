import type { Metadata } from "next";
import { JobDetail } from "@/features/ingestion/components/job-detail";

export const metadata: Metadata = { title: "Ingestion job" };

export default async function JobPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <JobDetail id={id} />;
}
