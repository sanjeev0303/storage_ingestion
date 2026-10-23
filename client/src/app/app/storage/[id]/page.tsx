import { ConnectionDetail } from "@/features/storage-connections/components/connection-detail";

export default async function ConnectionPage({ params }: PageProps<"/app/storage/[id]">) {
  const { id } = await params;
  return <ConnectionDetail id={id} />;
}
