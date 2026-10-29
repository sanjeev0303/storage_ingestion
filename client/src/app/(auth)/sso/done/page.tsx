import type { Metadata } from "next";
import { Suspense } from "react";
import { SsoDone } from "./sso-done";

export const metadata: Metadata = { title: "Signing in" };

export default function Page() {
  return (
    <Suspense>
      <SsoDone />
    </Suspense>
  );
}
