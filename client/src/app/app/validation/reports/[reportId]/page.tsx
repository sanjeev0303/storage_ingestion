import type { Metadata } from "next";
import { ReportScreen } from "@/features/validation/components/report-screen";

export const metadata: Metadata = { title: "Validation report" };

export default async function ReportPage({ params }: { params: Promise<{ reportId: string }> }) {
  const { reportId } = await params;
  return <ReportScreen reportId={reportId} />;
}
