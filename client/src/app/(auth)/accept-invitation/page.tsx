import type { Metadata } from "next";
import { Suspense } from "react";
import { TokenForm } from "@/components/auth/token-form";

export const metadata: Metadata = { title: "Accept an invitation" };

export default function AcceptInvitationPage() {
  return (
    <Suspense>
      <TokenForm
        flow={{
          title: "Accept an invitation",
          intro: "Paste the invitation token you received. New people set a password; existing accounts join as they are.",
          path: "/invitations/accept",
          button: "Accept invitation",
          done: "Invitation accepted",
          withName: true,
          passwordOptional: true,
        }}
      />
    </Suspense>
  );
}
