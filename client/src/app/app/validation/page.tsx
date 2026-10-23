import type { Metadata } from "next";
import { ValidationIndex } from "@/features/validation/components/validation-index";

export const metadata: Metadata = { title: "Validation" };

export default function ValidationPage() {
  return <ValidationIndex />;
}
