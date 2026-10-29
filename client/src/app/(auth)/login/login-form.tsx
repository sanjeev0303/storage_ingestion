"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { Field, Input } from "@/components/ui/field";
import { PasswordInput } from "@/components/ui/password-field";
import { SubmitButton } from "@/components/ui/submit-button";
import { Panel } from "@/components/ui/panel";
import { describeError, readError } from "@/lib/api/errors";
import { safeNext } from "@/lib/auth/redirect";
import { zodResolver } from "@/lib/validation/resolver";
import { useState } from "react";
import { SsoForm } from "./sso-form";

const schema = z.object({
  email: z.string().trim().toLowerCase().min(1, "Enter your email address.").email("Enter a valid email address."),
  password: z.string().min(1, "Enter your password."),
});
type Values = z.infer<typeof schema>;

function messageFor(code: string | undefined, fallback: string, retryAfter?: number) {
  switch (code) {
    case "invalid_credentials":
      return "The email or password is wrong.";
    case "account_inactive":
      return "This account is not active. Ask a platform administrator to activate or reactivate it.";
    case "rate_limited":
      return retryAfter ? `Too many attempts. Try again in ${retryAfter} seconds.` : "Too many attempts. Try again shortly.";
    default:
      return fallback;
  }
}

export function LoginForm({ ssoError }: { ssoError?: string }) {
  const router = useRouter();
  const params = useSearchParams();
  const [formError, setFormError] = useState<string | null>(null);
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting, isValid },
  } = useForm<Values>({
    resolver: zodResolver<Values>(schema),
    mode: "onTouched",
    // A link can carry the address, for example from an invitation or a reset email.
    defaultValues: { email: params.get("email") ?? "", password: "" },
  });

  async function onSubmit(values: Values) {
    setFormError(null);
    try {
      const res = await fetch("/api/session/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(values),
        credentials: "same-origin",
      });
      if (!res.ok) {
        const error = await readError(res);
        setFormError(messageFor(error.code, describeError(error), error.retryAfter));
        return;
      }
      router.replace(safeNext(params.get("next")));
    } catch {
      setFormError("Could not reach the server. Check the connection and try again.");
    }
  }

  return (
    <Panel as="div" className="w-full max-w-md" padded>
      <h1 className="text-2xl font-semibold tracking-tight">Sign in</h1>
      <p className="mt-1 text-base text-muted">Use the account your organization gave you.</p>

      <form onSubmit={handleSubmit(onSubmit)} noValidate className="mt-6 flex flex-col gap-5">
        <Field label="Email" error={errors.email?.message}>
          {(p) => <Input type="email" autoComplete="username" autoCapitalize="none" spellCheck={false} autoFocus {...p} {...register("email")} />}
        </Field>
        <Field label="Password" error={errors.password?.message}>
          {(p) => <PasswordInput autoComplete="current-password" {...p} {...register("password")} />}
        </Field>

        {formError && (
          <p role="alert" className="text-base font-medium">
            {formError}
          </p>
        )}

        <SubmitButton valid={isValid} loading={isSubmitting} className="h-10" hint="Enter your email and password to continue.">
          Sign in
        </SubmitButton>
      </form>

      <div className="mt-10">
        <h2 className="text-base font-semibold">Single sign-on</h2>
        <p className="mt-1 mb-4 text-sm text-muted">If your organization signs in through its own identity provider.</p>
        <SsoForm refused={ssoError} />
      </div>

      <p className="mt-6 text-sm text-muted">
        New here?{" "}
        <Link href="/register" className="font-medium text-ink underline underline-offset-2">
          Create an account
        </Link>
        . Have an invitation?{" "}
        <Link href="/accept-invitation" className="font-medium text-ink underline underline-offset-2">
          Accept it
        </Link>
        . Need to set a first password?{" "}
        <Link href="/activate" className="font-medium text-ink underline underline-offset-2">
          Activate your account
        </Link>
        . Locked out?{" "}
        <Link href="/reset-password" className="font-medium text-ink underline underline-offset-2">
          Use a reset token
        </Link>
        .
      </p>
    </Panel>
  );
}
