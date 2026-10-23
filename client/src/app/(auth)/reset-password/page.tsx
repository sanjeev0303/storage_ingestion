import type { Metadata } from "next";
import { Suspense } from "react";
import { TokenForm } from "@/components/auth/token-form";

export const metadata: Metadata = { title: "Reset your password" };

export default function ResetPasswordPage() {
  return (
    <Suspense>
      <TokenForm
        flow={{
          title: "Reset your password",
          intro: "A platform administrator issues a reset token. Paste it here and choose a new password.",
          path: "/auth/password/reset",
          button: "Set new password",
          done: "Password changed",
        }}
      />
    </Suspense>
  );
}
