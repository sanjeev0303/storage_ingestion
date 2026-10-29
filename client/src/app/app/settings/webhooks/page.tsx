import type { Metadata } from "next";
import { WebhooksScreen } from "@/features/webhooks/components/webhooks-screen";

export const metadata: Metadata = { title: "Webhooks" };

export default function Page() {
  return <WebhooksScreen />;
}
