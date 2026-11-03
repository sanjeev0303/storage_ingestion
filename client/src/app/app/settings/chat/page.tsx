import type { Metadata } from "next";
import { ChatSettings } from "@/features/connectors/components/chat-settings";

export const metadata: Metadata = { title: "Chat" };

export default function ChatSettingsPage() {
  return <ChatSettings />;
}
