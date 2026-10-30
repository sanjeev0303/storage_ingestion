import type { Metadata } from "next";
import { ReviewsScreen } from "@/features/evidence/components/reviews-screen";

export const metadata: Metadata = { title: "Exception reviews" };

export default function AuditReviewsPage() {
  return <ReviewsScreen />;
}
