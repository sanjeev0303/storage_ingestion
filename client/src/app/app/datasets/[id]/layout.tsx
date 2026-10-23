import { DatasetShell } from "@/features/datasets/components/dataset-shell";

export default async function DatasetLayout({ children, params }: LayoutProps<"/app/datasets/[id]">) {
  const { id } = await params;
  return <DatasetShell id={id}>{children}</DatasetShell>;
}
