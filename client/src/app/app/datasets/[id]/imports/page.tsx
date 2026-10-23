import type { Metadata } from "next";
import { ImportsScreen } from "@/features/datasets/components/imports-screen";

export const metadata: Metadata = { title: "Manifest imports" };

export default async function ImportsPage({ params }: PageProps<"/app/datasets/[id]/imports">) {
  const { id } = await params;
  return <ImportsScreen id={id} />;
}
