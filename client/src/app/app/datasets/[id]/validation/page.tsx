import type { Metadata } from "next";
import { DatasetValidationScreen } from "@/features/validation/components/dataset-validation-screen";

export const metadata: Metadata = { title: "Dataset validation" };

type Props = { params: Promise<{ id: string }>; searchParams: Promise<{ version?: string }> };

export default async function DatasetValidationPage({ params, searchParams }: Props) {
  const { id } = await params;
  const { version } = await searchParams;
  return <DatasetValidationScreen datasetId={id} versionId={version} />;
}
