import { PageHeader, TabNav } from "@/components/ui/page";

const tabs = [
  { href: "/app/ingestion/uploads", label: "Uploads" },
  { href: "/app/ingestion/jobs", label: "Bulk ingestion" },
];

export default function IngestionLayout({ children }: LayoutProps<"/app/ingestion">) {
  return (
    <>
      <PageHeader
        title="Ingestion"
        description="Upload files one at a time, or load a whole dataset from a manifest."
      />
      <TabNav tabs={tabs} label="Ingestion" />
      {children}
    </>
  );
}
