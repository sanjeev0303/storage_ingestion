import type { Metadata } from "next";
import { NewJobForm } from "@/features/ingestion/components/new-job-form";

export const metadata: Metadata = { title: "New ingestion job" };

export default function NewJobPage() {
  return <NewJobForm />;
}
