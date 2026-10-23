import { PageHeader } from "@/components/ui/page";
import { CreateConnection } from "@/features/storage-connections/components/create-connection";

export default function NewConnectionPage() {
  return (
    <>
      <PageHeader
        title="Add a storage connection"
        crumbs={[{ label: "Storage", href: "/app/storage" }, { label: "Add connection" }]}
        description="Connect your own bucket or container. Creating the connection does not contact the provider; you test it next."
      />
      <CreateConnection />
    </>
  );
}
