import type { Metadata } from "next";
import { cookies } from "next/headers";
import { Suspense } from "react";
import { SSO_ERROR_COOKIE } from "@/server/sso";
import { LoginForm } from "./login-form";

export const metadata: Metadata = { title: "Sign in" };

export default async function LoginPage() {
  // Why a single sign-on was refused, set by its callback for a minute.
  const ssoError = (await cookies()).get(SSO_ERROR_COOKIE)?.value;
  return (
    <Suspense>
      <LoginForm ssoError={ssoError} />
    </Suspense>
  );
}
