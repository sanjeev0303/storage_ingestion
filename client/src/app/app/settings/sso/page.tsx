import type { Metadata } from "next";
import { SsoScreen } from "@/features/sso/components/sso-screen";

export const metadata: Metadata = { title: "Single sign-on" };

export default function Page() {
  return <SsoScreen />;
}
