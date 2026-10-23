import type { Metadata } from "next";
import { ProfileForm } from "@/features/validation/components/profile-form";

export const metadata: Metadata = { title: "New validation profile" };

export default function NewProfilePage() {
  return <ProfileForm />;
}
