import type { Metadata } from "next";
import { Suspense } from "react";
import { PageHeader } from "@/components/ui/page";
import { CatalogList } from "@/features/catalog/components/catalog-list";

export const metadata: Metadata = { title: "Catalog" };

export default function CatalogPage() {
  return (
    <>
      <PageHeader
        title="Catalog"
        description="Every object in the organization, with its current version and checksum. Filters are kept in the address, so a filtered view can be shared."
      />
      <Suspense>
        <CatalogList />
      </Suspense>
    </>
  );
}
