import type { Metadata } from "next";
import { DatasetOverview } from "@/features/datasets/components/dataset-overview";

export const metadata: Metadata = { title: "Dataset" };

export default async function DatasetPage({ params }: PageProps<"/app/datasets/[id]">) {
  const { id } = await params;
  return <DatasetOverview id={id} />;
}
