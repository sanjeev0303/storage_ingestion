import type { Metadata } from "next";
import { Suspense } from "react";
import { ObjectDetail } from "@/features/catalog/components/object-detail";

export const metadata: Metadata = { title: "Catalog object" };

export default async function CatalogObjectPage({ params }: PageProps<"/app/catalog/[id]">) {
  const { id } = await params;
  return (
    <Suspense>
      <ObjectDetail id={id} />
    </Suspense>
  );
}
