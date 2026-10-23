import type { Metadata } from "next";
import { DeadLettersScreen } from "@/features/operations/components/dead-letters-screen";

export const metadata: Metadata = { title: "Dead letters" };

export default function DeadLettersPage() {
  return <DeadLettersScreen />;
}
