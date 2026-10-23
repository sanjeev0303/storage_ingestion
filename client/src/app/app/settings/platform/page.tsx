import type { Metadata } from "next";
import { PlatformAdmin } from "@/features/organization/components/platform";

export const metadata: Metadata = { title: "Platform administration" };

export default function Page() {
  return <PlatformAdmin />;
}
