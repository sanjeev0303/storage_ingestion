import type { Metadata } from "next";
import { ProfileDetail } from "@/features/validation/components/profile-detail";

export const metadata: Metadata = { title: "Validation profile" };

type Props = { params: Promise<{ profileId: string }>; searchParams: Promise<{ version?: string }> };

export default async function ProfilePage({ params, searchParams }: Props) {
  const { profileId } = await params;
  const { version } = await searchParams;
  const parsed = Number(version);
  return <ProfileDetail profileId={decodeURIComponent(profileId)} version={Number.isInteger(parsed) && parsed > 0 ? parsed : undefined} />;
}
