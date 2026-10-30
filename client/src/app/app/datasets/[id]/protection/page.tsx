import type { Metadata } from "next";
import { ProtectionScreen } from "@/features/protection/components/protection-screen";

export const metadata: Metadata = { title: "Dataset protection" };

export default async function ProtectionPage({ params }: PageProps<"/app/datasets/[id]/protection">) {
  const { id } = await params;
  return <ProtectionScreen id={id} />;
}
