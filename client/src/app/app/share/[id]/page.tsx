import type { Metadata } from "next";
import { SharePage } from "@/features/connectors/components/share-page";

export const metadata: Metadata = { title: "Shared file" };

export default async function ChatSharePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <SharePage id={id} />;
}
