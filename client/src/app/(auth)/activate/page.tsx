import type { Metadata } from "next";
import { Suspense } from "react";
import { TokenForm } from "@/components/auth/token-form";

export const metadata: Metadata = { title: "Activate your account" };

export default function ActivatePage() {
  return (
    <Suspense>
      <TokenForm
        flow={{
          title: "Activate your account",
          intro: "Paste the activation token an administrator gave you and choose a password.",
          path: "/auth/activate",
          button: "Activate account",
          done: "Account activated",
        }}
      />
    </Suspense>
  );
}
