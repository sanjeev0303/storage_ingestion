import type { Metadata } from "next";
import { CreateDatasetForm } from "@/features/datasets/components/create-dataset-form";

export const metadata: Metadata = { title: "New dataset" };

export default function NewDatasetPage() {
  return <CreateDatasetForm />;
}
