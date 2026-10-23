import type { Metadata } from "next";
import { LineageScreen } from "@/features/lineage/components/lineage-screen";

export const metadata: Metadata = { title: "Dataset lineage" };

type Props = { params: Promise<{ id: string }>; searchParams: Promise<{ version?: string }> };

export default async function DatasetLineagePage({ params, searchParams }: Props) {
  const { id } = await params;
  const { version } = await searchParams;
  return <LineageScreen datasetId={id} versionId={version} />;
}
