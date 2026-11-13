import { DatasetShell } from "@/features/datasets/components/dataset-shell";

export default async function DatasetLayout({ children, params }: LayoutProps<"/app/datasets/[id]">) {
  const { id } = await params;
  // keyed so a link to another dataset, which Next renders without remounting, starts every tab fresh
  return <DatasetShell key={id} id={id}>{children}</DatasetShell>;
}
