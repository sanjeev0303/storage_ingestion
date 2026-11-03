import type { Metadata } from "next";
import { ConnectScreen } from "@/features/connectors/components/connect-screen";

export const metadata: Metadata = { title: "Connect" };

export default async function DatasetConnectPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <ConnectScreen id={id} />;
}
