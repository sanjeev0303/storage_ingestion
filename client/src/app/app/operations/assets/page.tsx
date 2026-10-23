import type { Metadata } from "next";
import { AssetsScreen } from "@/features/operations/components/assets-screen";

export const metadata: Metadata = { title: "Assets" };

export default async function AssetsPage({ searchParams }: PageProps<"/app/operations/assets">) {
  const { id } = await searchParams;
  const initialId = typeof id === "string" ? id : "";
  // keyed so a link to another ?id= while this page is open starts a fresh lookup
  return <AssetsScreen key={initialId} initialId={initialId} />;
}
