import type { Metadata } from "next";
import { DatasetsList } from "@/features/datasets/components/datasets-list";

export const metadata: Metadata = { title: "Datasets" };

export default function DatasetsPage() {
  return <DatasetsList />;
}
