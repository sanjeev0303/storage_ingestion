import { PageHeader, TabNav } from "@/components/ui/page";

const tabs = [
  { href: "/app/ingestion/uploads", label: "Uploads" },
  { href: "/app/ingestion/jobs", label: "Bulk ingestion" },
  { href: "/app/ingestion/adopt", label: "Adopt existing data" },
];

export default function IngestionLayout({ children }: LayoutProps<"/app/ingestion">) {
  return (
    <>
      <PageHeader
        title="Ingestion"
        description="Upload files one at a time, load a whole dataset from a manifest, or adopt data already in your bucket."
      />
      <TabNav tabs={tabs} label="Ingestion" />
      {children}
    </>
  );
}
