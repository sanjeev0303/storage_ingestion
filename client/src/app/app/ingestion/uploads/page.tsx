import type { Metadata } from "next";
import { UploadsScreen } from "@/features/uploads/components/uploads-screen";

export const metadata: Metadata = { title: "Uploads" };

export default function UploadsPage() {
  return <UploadsScreen />;
}
