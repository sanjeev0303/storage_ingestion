import type { Metadata } from "next";
import { JobScreen } from "@/features/validation/components/job-screen";

export const metadata: Metadata = { title: "Validation job" };

export default async function JobPage({ params }: { params: Promise<{ jobId: string }> }) {
  const { jobId } = await params;
  return <JobScreen jobId={jobId} />;
}
