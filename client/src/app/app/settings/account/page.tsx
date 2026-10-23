import type { Metadata } from "next";
import { AccountScreen } from "@/features/account/components/account-screen";

export const metadata: Metadata = { title: "Account" };

export default function Page() {
  return <AccountScreen />;
}
