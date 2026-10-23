import type { Metadata } from "next";
import { JobsList, NewJobButton } from "@/features/ingestion/components/jobs-list";

export const metadata: Metadata = { title: "Bulk ingestion" };

export default function JobsPage() {
  return (
    <>
      <div className="mb-5 flex justify-end">
        <NewJobButton />
      </div>
      <JobsList />
    </>
  );
}
